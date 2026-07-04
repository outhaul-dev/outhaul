package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

const (
	sessionCookie = "outhaul_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// NewToken returns a URL-safe random token (also used for the setup URL).
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal for security; surface loudly.
		panic("outhaul: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// requireAuth wraps a handler, redirecting unauthenticated browsers to /login.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentSession(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// currentSession returns the valid session for the request, if any.
func (s *Server) currentSession(r *http.Request) (core.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return core.Session{}, false
	}
	sess, err := s.store.GetSession(r.Context(), c.Value)
	if err != nil {
		return core.Session{}, false
	}
	return sess, true
}

// login creates a session and sets the cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request, user core.User) error {
	token := NewToken()
	if err := s.store.CreateSession(r.Context(), core.Session{
		Token:     token,
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(sessionTTL),
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	return nil
}

// --- first-boot setup ---

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	done, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if done {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !s.validSetupToken(r) {
		s.render(w, http.StatusForbidden, "setup", map[string]any{
			"Title": "Setup", "Invalid": true, "HideChrome": true,
		})
		return
	}
	s.render(w, http.StatusOK, "setup", map[string]any{
		"Title": "Setup", "Token": s.setupToken, "HideChrome": true,
	})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	done, err := s.store.HasUser(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if done {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !s.validSetupToken(r) {
		s.render(w, http.StatusForbidden, "setup", map[string]any{"Title": "Setup", "Invalid": true, "HideChrome": true})
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	if err := validateCredentials(username, password); err != nil {
		s.render(w, http.StatusBadRequest, "setup", map[string]any{
			"Title": "Setup", "Token": s.setupToken, "HideChrome": true, "Error": err.Error(),
		})
		return
	}

	hash, err := HashPassword(password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.store.CreateUser(r.Context(), username, hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.login(w, r, user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) validSetupToken(r *http.Request) bool {
	got := r.FormValue("token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return s.setupToken != "" && got == s.setupToken
}

// --- login / logout ---

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// If there is no admin yet, steer the operator to setup.
	if done, _ := s.store.HasUser(r.Context()); !done {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", map[string]any{"Title": "Sign in", "HideChrome": true})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := s.store.GetUserByName(r.Context(), username)
	if err != nil {
		s.loginFailed(w, r, err)
		return
	}
	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil || !ok {
		s.loginFailed(w, r, errors.New("invalid credentials"))
		return
	}
	if err := s.login(w, r, user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, cause error) {
	// Do not leak whether the username exists.
	_ = cause
	s.render(w, http.StatusUnauthorized, "login", map[string]any{
		"Title": "Sign in", "Error": "Invalid username or password.", "HideChrome": true,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// validateCredentials enforces minimal admin credential rules.
func validateCredentials(username, password string) error {
	if len(username) < 3 {
		return errors.New("username must be at least 3 characters")
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

// NeedsSetup reports whether no admin user exists yet.
func (s *Server) NeedsSetup(ctx context.Context) (bool, error) {
	has, err := s.store.HasUser(ctx)
	return !has, err
}

// SetupToken returns the current one-time setup token.
func (s *Server) SetupToken() string { return s.setupToken }
