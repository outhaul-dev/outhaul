package server

import (
	"net/http"
	"strconv"
	"strings"
)

// handleAttachDatabase links a project database to the app under a chosen env
// var. Env-var format, same-project, and uniqueness are validated in the store;
// PORT is rejected here because Outhaul manages it and the pipeline would
// silently drop an attachment targeting it.
func (s *Server) handleAttachDatabase(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetApp(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	dbID, ok := parseID(r.FormValue("database_id"))
	if !ok {
		http.Error(w, "select a database", http.StatusBadRequest)
		return
	}
	envVar := strings.TrimSpace(r.FormValue("env_var"))
	if envVar == "PORT" {
		http.Error(w, "PORT is managed by Outhaul and cannot be used for an attachment.", http.StatusBadRequest)
		return
	}
	if _, err := s.store.AttachDatabase(r.Context(), id, dbID, envVar); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleDetachDatabase removes one attachment; the database is untouched.
func (s *Server) handleDetachDatabase(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	attID, ok := parseID(r.PathValue("attachmentID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DetachDatabase(r.Context(), id, attID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
