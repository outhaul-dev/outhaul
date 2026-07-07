package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/github"
)

func TestGithubCallbackStoresApp(t *testing.T) {
	env := newTestEnv(t)
	env.gh.ManifestResult = github.ManifestResult{
		AppID: 55, Slug: "outhaul-t", PEM: "PEM", WebhookSecret: "whs",
		ClientID: "cid", ClientSecret: "csec",
	}

	state := env.srv.newGithubState()
	req := httptest.NewRequest("GET", "/github/callback?code=xyz&state="+state, nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "github.com/apps/outhaul-t/installations/new") {
		t.Errorf("redirect = %q, want install URL", loc)
	}
	if env.gh.LastCode != "xyz" {
		t.Errorf("exchanged code = %q", env.gh.LastCode)
	}
	if _, ok, _ := env.store.GithubApp(req.Context()); !ok {
		t.Error("github app not stored")
	}
}

func TestGithubCallbackRejectsBadState(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("GET", "/github/callback?code=xyz&state=forged", nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if env.gh.LastCode != "" {
		t.Error("exchanged code despite bad state")
	}
}

func TestGithubSetupStoresInstallation(t *testing.T) {
	env := newTestEnv(t)
	env.gh.ManifestResult = github.ManifestResult{AppID: 1, Slug: "s", PEM: "p", WebhookSecret: "w", ClientID: "c", ClientSecret: "cs"}
	// Seed an app record first.
	st := env.srv.newGithubState()
	env.srv.Handler().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/github/callback?code=c&state="+st, nil))

	form := url.Values{"installation_id": {"321"}, "setup_action": {"install"}}
	req := httptest.NewRequest("GET", "/github/setup?"+form.Encode(), nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	ga, _, _ := env.store.GithubApp(req.Context())
	if ga.InstallationID != 321 {
		t.Errorf("installation id = %d, want 321", ga.InstallationID)
	}
}

// TestGithubCallbackAndSetupWorkWithoutSession verifies these GitHub-facing
// endpoints are NOT behind requireAuth: no session cookie is ever set on the
// requests above, and they still succeed (rather than 303 to /login).
func TestGithubConnectRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	resp := env.get(t, "/github/connect")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("connect without session = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}
