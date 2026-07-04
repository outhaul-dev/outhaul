package server

import (
	"net/http"
)

// handleSettings renders the settings hub: GitHub App connection status and the
// change-password form.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Title": "Settings", "Active": "settings"}
	if !s.publicURLSet() {
		data["NeedsPublicURL"] = true
	}
	if ga, ok, err := s.store.GithubApp(r.Context()); err == nil && ok {
		data["GithubSlug"] = ga.Slug
		data["GithubInstalled"] = ga.InstallationID != 0
	}
	if r.URL.Query().Get("ok") != "" {
		data["Notice"] = "Password updated."
	}
	s.render(w, http.StatusOK, "settings", data)
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
