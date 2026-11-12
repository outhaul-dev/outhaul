package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
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

// requestIsHTTPS reports whether the browser reached us over HTTPS — directly,
// or through Traefik, which terminates TLS and sets X-Forwarded-Proto. A
// spoofed header only makes the spoofer's own cookie Secure.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionCookieFor is the session cookie for r, with no value or lifetime
// yet; every place that sets or clears it starts here.
//
// It is Secure when the request is HTTPS, and always on an HTTPS public URL's
// own host: in tunnel mode Traefik serves plain HTTP and says so in
// X-Forwarded-Proto while browsers are on HTTPS, so the request alone can't
// tell. Anywhere else (the admin port on a LAN address, an SSH tunnel to
// localhost) it follows the request, so plain-HTTP sign-in keeps working there.
func (s *Server) sessionCookieFor(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r) || (s.publicURLIsHTTPS() && s.onPublicHost(r)),
		SameSite: http.SameSiteLaxMode,
	}
}

// login creates a session and sets the cookie. It also starts a gateway
// session, so the sign-in covers protected routes; that covers first-boot
// setup and setup links.
func (s *Server) login(w http.ResponseWriter, r *http.Request, user core.User) error {
	token, expires, err := s.createSession(r.Context(), user)
	if err != nil {
		return err
	}
	gateToken, err := s.startGateSession(w, r, user.GateSubject(), user.SignIn, user.Display())
	if err != nil {
		if err := s.store.DeleteSession(context.WithoutCancel(r.Context()), token); err != nil {
			log.Printf("server: ending user %d's session after its gateway session failed: %v", user.ID, err)
		}
		return err
	}
	s.endReplacedGateSession(r, gateToken)
	s.finishLogin(w, r, user, token, expires)
	return nil
}

// createSession stores a new session for the account. finishLogin sets its
// cookie.
func (s *Server) createSession(ctx context.Context, user core.User) (token string, expires time.Time, err error) {
	sess := newSession(user)
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return "", time.Time{}, err
	}
	return sess.Token, sess.ExpiresAt, nil
}

// newSession is a new session for the account, not yet stored. An SSO session
// lasts ssoSessionTTL, a password session sessionTTL.
func newSession(user core.User) core.Session {
	ttl := sessionTTL
	if user.SignIn == core.SignInSSO {
		ttl = ssoSessionTTL
	}
	return core.Session{Token: NewToken(), UserID: user.ID, ExpiresAt: time.Now().Add(ttl)}
}

// finishLogin records the sign-in and sets the session cookie, which lasts
// as long as the session.
func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, user core.User, token string, expires time.Time) {
	// Recording the time is for the Users page; it must not block signing in.
	if err := s.store.TouchUserSignIn(r.Context(), user.ID); err != nil {
		log.Printf("server: record sign-in for user %d: %v", user.ID, err)
	}
	c := s.sessionCookieFor(r)
	c.Value, c.Expires = token, expires
	http.SetCookie(w, c)
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
		s.render(w, r, http.StatusForbidden, "setup", map[string]any{
			"Title": "Setup", "Invalid": true, "HideChrome": true,
		})
		return
	}
	s.render(w, r, http.StatusOK, "setup", map[string]any{
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
		s.render(w, r, http.StatusForbidden, "setup", map[string]any{"Title": "Setup", "Invalid": true, "HideChrome": true})
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	err = ValidateCredentials(username, password)
	// A password account never takes a name that looks like an SSO account's.
	if err == nil && hasSSOPrefix(username) {
		err = errors.New(ssoPrefixRefusal)
	}
	if err != nil {
		s.render(w, r, http.StatusBadRequest, "setup", map[string]any{
			"Title": "Setup", "Token": s.setupToken, "HideChrome": true, "Error": err.Error(),
		})
		return
	}

	var hash string
	if busy := s.withHashSlot(r.Context(), func() { hash, err = HashPassword(password) }); busy != nil {
		s.render(w, r, http.StatusServiceUnavailable, "setup", map[string]any{
			"Title": "Setup", "Token": s.setupToken, "HideChrome": true, "Error": busyMessage,
		})
		return
	}
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
	// Only a known code shows anything, and only its fixed message.
	s.loginPage(w, r, http.StatusOK, ssoLoginErrors[r.URL.Query().Get("error")])
}

// loginPage renders the sign-in page, with the SSO button when a provider is
// connected.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	data := map[string]any{"Title": "Sign in", "HideChrome": true, "SSO": s.ssoConnected(r.Context())}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	s.render(w, r, status, "login", data)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	user, err := s.checkPassword(r.Context(), r.FormValue("username"), r.FormValue("password"))
	var locked *signInLockedError
	if errors.As(err, &locked) {
		w.Header().Set("Retry-After", strconv.Itoa(locked.retryAfter()))
		s.loginPage(w, r, http.StatusTooManyRequests, locked.Error())
		return
	}
	if errors.Is(err, errHashBusy) {
		s.loginPage(w, r, http.StatusServiceUnavailable, busyMessage)
		return
	}
	if err != nil {
		s.loginFailed(w, r, err)
		return
	}
	// The sessions are made only if the account still has the hash just
	// checked: a reset or change that committed during the check has ended
	// the account's sessions, and these mustn't outlive it.
	sess := newSession(user)
	err = s.store.CreatePasswordSession(r.Context(), sess, user.PasswordHash)
	if errors.Is(err, store.ErrPasswordChanged) {
		s.loginFailed(w, r, err)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The sign-in covers protected routes too.
	gateToken, err := s.startPasswordGateSession(w, r, user)
	if err != nil {
		if err := s.store.DeleteSession(context.WithoutCancel(r.Context()), sess.Token); err != nil {
			log.Printf("server: ending user %d's session after its gateway session failed: %v", user.ID, err)
		}
		if errors.Is(err, store.ErrPasswordChanged) {
			s.loginFailed(w, r, err)
			return
		}
		log.Printf("server: starting user %d's gateway session: %v", user.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.endReplacedGateSession(r, gateToken)
	s.finishLogin(w, r, user, sess.Token, sess.ExpiresAt)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, cause error) {
	// Do not leak whether the username exists.
	_ = cause
	s.loginPage(w, r, http.StatusUnauthorized, "Invalid username or password.")
}

// handleLogout ends the Outhaul session and the gateway session, so the
// person also loses every protected route they passed in this browser.
//
// The gateway session goes first. If it can't be ended, nothing is: the
// person is told signing out didn't work, keeps both sessions and their
// cookies, and can try again, rather than leaving with route access they
// think has ended.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.endGateSession(w, r); err != nil {
		log.Printf("server: ending a gateway session at sign-out: %v", err)
		http.Error(w, gateSignOutFailedCopy, http.StatusServiceUnavailable)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	c := s.sessionCookieFor(r)
	c.MaxAge = -1
	http.SetCookie(w, c)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// busyMessage is shown when every password-hashing slot stayed taken.
const busyMessage = "Too many sign-in attempts right now. Wait a moment and try again."

// ValidateCredentials enforces the minimal username and password rules.
func ValidateCredentials(username, password string) error {
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
