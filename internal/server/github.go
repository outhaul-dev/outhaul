package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

// errNoOwningApp is returned by bindInstallation when no connected GitHub App
// owns the installation. It is the only bindInstallation error safe to show a
// caller: it carries no store/driver detail, unlike every other failure path.
var errNoOwningApp = errors.New("no connected GitHub App owns this installation")

// orgLogin matches a GitHub account name: alphanumerics and single hyphens,
// no leading or trailing hyphen.
var orgLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// handleGithubConnect asks where the App should be created, then renders the
// auto-submitting manifest form pointed at that account's App form. A private
// App can only be installed on the account that owns it, so this choice is what
// decides which account the resulting source covers.
func (s *Server) handleGithubConnect(w http.ResponseWriter, r *http.Request) {
	if !s.publicURLSet() {
		s.render(w, http.StatusOK, "github_connect", map[string]any{
			"Title": "Connect GitHub", "Active": "settings", "NeedsPublicURL": true,
		})
		return
	}
	owner := r.URL.Query().Get("owner")
	org := strings.TrimSpace(r.URL.Query().Get("org"))

	// No choice made yet: show the picker.
	if owner == "" {
		s.render(w, http.StatusOK, "github_connect", map[string]any{
			"Title": "Connect GitHub", "Active": "settings", "Choose": true,
		})
		return
	}
	action := "https://github.com/settings/apps/new"
	if owner == "org" {
		if !orgLogin.MatchString(org) {
			http.Error(w, "Enter a valid GitHub organization name.", http.StatusBadRequest)
			return
		}
		action = "https://github.com/organizations/" + org + "/settings/apps/new"
	}
	manifest, err := github.BuildManifest(github.ManifestParams{
		Name:      "outhaul-" + s.newNameSuffix(),
		PublicURL: s.publicURL,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "github_connect", map[string]any{
		"Title":    "Connect GitHub",
		"Active":   "settings",
		"Action":   action + "?state=" + s.newGithubState(),
		"Manifest": manifest,
	})
}

// handleGithubCallback exchanges the manifest code and records a new source.
// The row is persisted before installation on purpose: a restart between here
// and setup must not strand credentials for an App that now exists on GitHub.
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
	if _, err := s.store.CreateGithubAppSource(r.Context(), core.GithubAppCreds{
		AppID: res.AppID, Slug: res.Slug, PrivateKey: res.PEM,
		WebhookSecret: res.WebhookSecret, ClientID: res.ClientID, ClientSecret: res.ClientSecret,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "https://github.com/apps/"+res.Slug+"/installations/new", http.StatusSeeOther)
}

// handleGithubSetup records the installation the operator just created.
func (s *Server) handleGithubSetup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "missing installation_id", http.StatusBadRequest)
		return
	}
	if _, err := s.bindInstallation(r.Context(), id); err != nil {
		if errors.Is(err, errNoOwningApp) {
			http.Error(w, errNoOwningApp.Error(), http.StatusBadRequest)
			return
		}
		// A store or bind failure is our fault, not the caller's, and this route
		// is unauthenticated — never echo internal/driver detail into the body.
		log.Printf("github setup: bind installation %d: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// bindInstallation matches an installation id back to the source whose App owns
// it. GitHub sends no state to the setup URL, so instead of guessing we ask:
// GET /app/installations/{id} is scoped to the calling App, so only the owner
// gets an answer — and that answer carries the account name we want anyway.
//
// A source that already holds this installation is simply refreshed, which is
// the setup_on_update re-install path.
func (s *Server) bindInstallation(ctx context.Context, installationID int64) (core.GitSource, error) {
	sources, err := s.store.ListGitSources(ctx)
	if err != nil {
		return core.GitSource{}, err
	}
	// Already-bound source first, then pending ones newest-first: a retry of a
	// re-install must refresh rather than claim an unrelated pending App.
	var candidates []core.GitSource
	for _, src := range sources {
		if src.Kind == core.GitSourceGithubApp && src.GithubApp.InstallationID == installationID {
			candidates = append(candidates, src)
		}
	}
	for i := len(sources) - 1; i >= 0; i-- {
		if sources[i].Kind == core.GitSourceGithubApp && sources[i].GithubApp.InstallationID == 0 {
			candidates = append(candidates, sources[i])
		}
	}
	for _, src := range candidates {
		jwt, err := github.AppJWT(src.GithubApp.PrivateKey, src.GithubApp.AppID, time.Now())
		if err != nil {
			log.Printf("github setup: app jwt for %s: %v", src.Display(), err)
			continue
		}
		inst, err := s.gh.Installation(ctx, jwt, installationID)
		if err != nil {
			continue // this App does not own the installation
		}
		if err := s.store.BindGithubInstallation(ctx, src.ID, installationID, inst.AccountLogin, inst.AccountType); err != nil {
			return core.GitSource{}, err
		}
		src.GithubApp.InstallationID = installationID
		src.AccountLogin, src.AccountType = inst.AccountLogin, inst.AccountType
		return src, nil
	}
	return core.GitSource{}, errNoOwningApp
}

func (s *Server) publicURLSet() bool { return s.publicURL != "" }

// newNameSuffix returns a short random suffix for a globally-unique App name.
func (s *Server) newNameSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
