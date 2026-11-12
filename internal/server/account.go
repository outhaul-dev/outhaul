package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
	"golang.org/x/crypto/ssh"
)

// accountPage wraps the /account routes. The guard lets in anyone signed in,
// because a password account with no role still needs to change its password.
// An SSO account with no role has nothing there: it sees only the no-access
// page, as everywhere else.
func (s *Server) accountPage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if me, _ := userFrom(r); me.SignIn != core.SignInPassword && !me.Role.AtLeast(core.RoleProjectViewer) {
			s.render(w, r, http.StatusForbidden, "noaccess", map[string]any{"Title": "No access", "HideChrome": true})
			return
		}
		h(w, r)
	}
}

// ssoHasNoPassword answers a password change from an SSO account.
const ssoHasNoPassword = "You sign in with SSO, so you have no password to change here."

// handleAccount shows the signed-in person's own page: their password and
// their push keys. Everyone can reach it, whatever their role, except an SSO
// account with no role (see accountPage).
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, http.StatusOK, "")
}

// renderAccount renders the account page, also to redisplay it with an error
// after a rejected push key.
func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	s.renderAccountWith(w, r, status, errMsg, nil)
}

// renderAccountWith is renderAccount with extra template data, such as a new
// push token to show once.
func (s *Server) renderAccountWith(w http.ResponseWriter, r *http.Request, status int, errMsg string, extra map[string]any) {
	me, _ := userFrom(r)
	keys, err := s.store.ListPushKeysForUser(r.Context(), me.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{"Title": "Account", "Active": "account", "PushKeys": keys}
	if r.URL.Query().Get("ok") != "" {
		data["Notice"] = "Password updated."
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	if s.gitHTTPOn() {
		if !s.addPushTokenData(w, r, me, data) {
			return
		}
	}
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, r, status, "account", data)
}

// handleChangePassword verifies the current password and stores a new hash.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	// The access guard has signed the person in and loaded their account.
	user, _ := userFrom(r)
	if user.SignIn != core.SignInPassword {
		http.Error(w, ssoHasNoPassword, http.StatusBadRequest)
		return
	}
	current := r.FormValue("current")
	next := r.FormValue("new")
	if len(next) < 8 {
		http.Error(w, "New password must be at least 8 characters.", http.StatusBadRequest)
		return
	}
	var sessionToken string
	if c, err := r.Cookie(sessionCookie); err == nil {
		sessionToken = c.Value
	}
	var err error
	var (
		valid     bool
		verifyErr error
		hash      string
	)
	busy := s.withHashSlot(r.Context(), func() {
		if valid, verifyErr = VerifyPassword(user.PasswordHash, current); verifyErr == nil && valid {
			hash, err = HashPassword(next)
		}
	})
	if busy != nil {
		http.Error(w, busyMessage, http.StatusServiceUnavailable)
		return
	}
	if verifyErr != nil || !valid {
		http.Error(w, "Current password is incorrect.", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Other sessions are revoked with the old password: a password change is
	// how an admin locks out a leaked session.
	if err := s.store.UpdateUserPassword(r.Context(), user.ID, hash, sessionToken); errors.Is(err, store.ErrNotPasswordAccount) {
		http.Error(w, ssoHasNoPassword, http.StatusBadRequest)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account?ok=1", http.StatusSeeOther)
}

// handleAddPushKey validates an authorized_keys line, derives its SHA256
// fingerprint, and stores it.
func (s *Server) handleAddPushKey(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.FormValue("label"))
	keyText := strings.TrimSpace(r.FormValue("public_key"))
	if label == "" || keyText == "" {
		s.renderAccount(w, r, http.StatusBadRequest, "Label and public key are required.")
		return
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyText))
	if err != nil {
		s.renderAccount(w, r, http.StatusBadRequest, "That does not look like a valid SSH public key.")
		return
	}
	fp := ssh.FingerprintSHA256(pub)
	me, _ := userFrom(r)
	if _, err := s.store.AddPushKey(r.Context(), core.PushKey{
		UserID:      me.ID,
		Label:       label,
		Fingerprint: fp,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
	}); err != nil {
		s.renderAccount(w, r, http.StatusBadRequest, "Could not add key (is it already registered?).")
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleDeletePushKey removes one of the signed-in account's keys. Someone
// else's key id deletes nothing.
func (s *Server) handleDeletePushKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	me, _ := userFrom(r)
	if err := s.store.DeletePushKeyForUser(r.Context(), me.ID, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
