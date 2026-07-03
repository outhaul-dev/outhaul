package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/github"
)

// handleGithubConnect renders the auto-submitting manifest form (or a notice if
// no public URL is configured).
func (s *Server) handleGithubConnect(w http.ResponseWriter, r *http.Request) {
	if !s.publicURLSet() {
		s.render(w, http.StatusOK, "github_connect", map[string]any{
			"Title": "Connect GitHub", "NeedsPublicURL": true,
		})
		return
	}
	manifest, err := github.BuildManifest(github.ManifestParams{
		Name:      "slipway-" + s.newNameSuffix(),
		PublicURL: s.publicURL,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state := s.newGithubState()
	action := "https://github.com/settings/apps/new?state=" + state
	if org := r.URL.Query().Get("org"); org != "" {
		action = "https://github.com/organizations/" + org + "/settings/apps/new?state=" + state
	}
	s.render(w, http.StatusOK, "github_connect", map[string]any{
		"Title":    "Connect GitHub",
		"Action":   action,
		"Manifest": manifest,
	})
}

// handleGithubCallback exchanges the manifest code and stores the App.
func (s *Server) handleGithubCallback(w http.ResponseWriter, r *http.Request) {
	if !s.consumeGithubState(r.URL.Query().Get("state")) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	res, err := s.gh.ExchangeManifest(r.Context(), code)
	if err != nil {
		http.Error(w, "manifest exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.store.SetGithubApp(r.Context(), core.GithubApp{
		AppID: res.AppID, Slug: res.Slug, PrivateKey: res.PEM,
		WebhookSecret: res.WebhookSecret, ClientID: res.ClientID, ClientSecret: res.ClientSecret,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "https://github.com/apps/"+res.Slug+"/installations/new", http.StatusSeeOther)
}

// handleGithubSetup records the installation id chosen during install.
func (s *Server) handleGithubSetup(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("installation_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "missing installation_id", http.StatusBadRequest)
		return
	}
	if err := s.store.SetInstallationID(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) publicURLSet() bool { return s.publicURL != "" }

// newNameSuffix returns a short random suffix for a globally-unique App name.
func (s *Server) newNameSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
