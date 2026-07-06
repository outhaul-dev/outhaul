package server

import (
	"net/http"
	"strings"

	"github.com/james-smart/outhaul/internal/core"
	"golang.org/x/crypto/ssh"
)

// handleSettings renders the settings hub: GitHub App connection status,
// backup destinations, and the change-password form.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "")
}

// renderSettings renders the settings page (also used to redisplay it with an
// error after a rejected destination change).
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	data := map[string]any{"Title": "Settings", "Active": "settings"}
	if !s.publicURLSet() {
		data["NeedsPublicURL"] = true
	}
	if ga, ok, err := s.store.GithubApp(r.Context()); err == nil && ok {
		data["GithubSlug"] = ga.Slug
		data["GithubInstalled"] = ga.InstallationID != 0
	}
	dests, err := s.store.ListDestinations(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["Destinations"] = dests
	keys, err := s.store.ListPushKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["PushKeys"] = keys
	data["SSHAddr"] = s.sshAddr()
	if r.URL.Query().Get("ok") != "" {
		data["Notice"] = "Password updated."
	}
	if name := r.URL.Query().Get("tested"); name != "" {
		data["Notice"] = "Destination " + name + " is reachable and writable."
	}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	s.render(w, status, "settings", data)
}

// handleChangePassword verifies the current password and stores a new hash.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	current := r.FormValue("current")
	next := r.FormValue("new")
	if len(next) < 8 {
		http.Error(w, "New password must be at least 8 characters.", http.StatusBadRequest)
		return
	}
	user, err := s.store.GetUser(r.Context(), sess.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	valid, err := VerifyPassword(user.PasswordHash, current)
	if err != nil || !valid {
		http.Error(w, "Current password is incorrect.", http.StatusBadRequest)
		return
	}
	hash, err := HashPassword(next)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), user.ID, hash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?ok=1", http.StatusSeeOther)
}

// handleAddPushKey validates an authorized_keys line, derives its SHA256
// fingerprint, and stores it.
func (s *Server) handleAddPushKey(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.FormValue("label"))
	keyText := strings.TrimSpace(r.FormValue("public_key"))
	if label == "" || keyText == "" {
		s.renderSettings(w, r, http.StatusBadRequest, "Label and public key are required.")
		return
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyText))
	if err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, "That does not look like a valid SSH public key.")
		return
	}
	fp := ssh.FingerprintSHA256(pub)
	if _, err := s.store.AddPushKey(r.Context(), core.PushKey{
		Label:       label,
		Fingerprint: fp,
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
	}); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, "Could not add key (is it already registered?).")
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleDeletePushKey removes a push key by ID.
func (s *Server) handleDeletePushKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeletePushKey(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSetSSHAddr persists and live-rebinds the git-push SSH listen address.
func (s *Server) handleSetSSHAddr(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.FormValue("ssh_addr"))
	if addr == "" || !strings.Contains(addr, ":") {
		s.renderSettings(w, r, http.StatusBadRequest, "Enter an address like :2222 or 0.0.0.0:2222.")
		return
	}
	if s.sshControl == nil {
		s.renderSettings(w, r, http.StatusServiceUnavailable, "The git-push SSH server is not running.")
		return
	}
	if err := s.sshControl.Rebind(addr); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, "Could not bind "+addr+": "+err.Error()+" (the previous port is still active).")
		return
	}
	if err := s.store.SetSetting(r.Context(), "ssh_addr", addr); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
