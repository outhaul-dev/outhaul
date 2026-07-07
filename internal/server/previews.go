package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/james-smart/outhaul/internal/core"
)

// handleSavePreviewConfig upserts an app's preview config from the app page.
func (s *Server) handleSavePreviewConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil || app.Source != core.SourceGithub {
		http.Error(w, "previews require a GitHub-connected app", http.StatusBadRequest)
		return
	}
	cfg := core.DefaultPreviewConfig(id)
	cfg.Enabled = r.FormValue("enabled") != ""
	cfg.BaseDomain = strings.TrimSpace(r.FormValue("base_domain"))
	cfg.PostPRComment = r.FormValue("post_pr_comment") != ""
	cfg.AllowForkPRs = r.FormValue("allow_fork_prs") != ""
	cfg.IdleTTLDays = atoiDefault(r.FormValue("idle_ttl_days"), 7)
	cfg.MaxConcurrent = atoiDefault(r.FormValue("max_concurrent"), 5)
	if err := s.store.SetPreviewConfig(r.Context(), cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleDestroyPreview manually tears down one preview child app.
func (s *Server) handleDestroyPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))                // parent
	previewID, ok2 := parseID(r.PathValue("previewID")) // child app id
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	if s.previews == nil {
		http.Error(w, "previews are not enabled on this server", http.StatusServiceUnavailable)
		return
	}
	if err := s.previews.DestroyByID(r.Context(), id, previewID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// atoiDefault parses s as a positive int, returning def on error or non-positive.
func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
