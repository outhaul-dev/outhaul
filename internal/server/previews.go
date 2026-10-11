package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
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
	// The base domain becomes part of every preview host, which lands in a
	// Traefik Host() rule and a local-CA cert filename: bare hostnames only.
	cfg.BaseDomain = strings.ToLower(strings.TrimSpace(r.FormValue("base_domain")))
	if cfg.BaseDomain != "" && !domainHostRe.MatchString(cfg.BaseDomain) {
		http.Error(w, "Preview base domain must be a bare hostname (e.g. preview.example.com).", http.StatusBadRequest)
		return
	}
	cfg.PostPRComment = r.FormValue("post_pr_comment") != ""
	cfg.AllowForkPRs = r.FormValue("allow_fork_prs") != ""
	cfg.IdleTTLDays = atoiDefault(r.FormValue("idle_ttl_days"), 7)
	cfg.MaxConcurrent = atoiDefault(r.FormValue("max_concurrent"), 5)
	if err := s.store.SetPreviewConfig(r.Context(), cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !cfg.AllowForkPRs {
		if err := s.destroyForkPreviews(context.WithoutCancel(r.Context()), id); err != nil {
			http.Error(w, "Saved, but "+err.Error()+" — the next hourly sweep will retry.", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// destroyForkPreviews tears down parentID's previews of fork PRs. Turning
// "Allow fork PRs" off must stop stranger code running now, not when each PR
// closes or idles out; same-repo previews are left alone.
func (s *Server) destroyForkPreviews(ctx context.Context, parentID int64) error {
	kids, err := s.store.ListPreviewsForParent(ctx, parentID)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if !k.IsForkPreview() {
			continue
		}
		if s.previews == nil {
			return fmt.Errorf("fork previews could not be torn down: previews are not enabled on this server")
		}
		if err := s.previews.DestroyByID(ctx, parentID, k.ID); err != nil {
			return fmt.Errorf("the fork preview for PR #%d could not be torn down: %w", k.PRNumber, err)
		}
	}
	return nil
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
