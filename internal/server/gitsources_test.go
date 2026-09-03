package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

// connectApp drives the manifest callback for one App and returns its source id.
func connectApp(t *testing.T, env *testEnv, appID int64, slug string) int64 {
	t.Helper()
	env.gh.ManifestResult = github.ManifestResult{
		AppID: appID, Slug: slug, PEM: testAppKeyPEM(t),
		WebhookSecret: "whs-" + slug, ClientID: "cid", ClientSecret: "csec",
	}
	state := env.srv.newGithubState()
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/callback?code=c&state="+state, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	src, ok, err := env.store.GitSourceByGithubAppID(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("source for app %d not stored: ok=%v err=%v", appID, ok, err)
	}
	return src.ID
}

func TestGithubCallbackCreatesAGitSource(t *testing.T) {
	env := newTestEnv(t)
	id := connectApp(t, env, 55, "outhaul-a")

	src, _, _ := env.store.GetGitSource(context.Background(), id)
	if src.Kind != core.GitSourceGithubApp {
		t.Errorf("kind = %q", src.Kind)
	}
	if src.Installed() {
		t.Error("a source must not be Installed before setup runs")
	}
}

func TestConnectingASecondAccountKeepsTheFirst(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	connectApp(t, env, 77, "outhaul-b")

	list, err := env.store.ListGitSources(context.Background())
	if err != nil {
		t.Fatalf("ListGitSources: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sources, want 2 — connecting an account must not replace the previous one", len(list))
	}
}

// Setup carries no state, so the installation is matched back to its App by
// asking GitHub which App owns it.
func TestGithubSetupBindsInstallationToTheOwningSource(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	wantID := connectApp(t, env, 77, "outhaul-b")

	env.gh.InstallationsByApp = map[int64][]github.Installation{
		77: {{ID: 9001, AccountLogin: "acme-corp", AccountType: "Organization"}},
	}
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/setup?installation_id=9001", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup status = %d, want 303; body=%s", rec.Code, rec.Body)
	}

	ctx := context.Background()
	bound, _, _ := env.store.GetGitSource(ctx, wantID)
	if !bound.Installed() || bound.GithubApp.InstallationID != 9001 {
		t.Errorf("owning source not bound: installation=%d", bound.GithubApp.InstallationID)
	}
	if bound.AccountLogin != "acme-corp" || bound.AccountType != "Organization" {
		t.Errorf("account = %q/%q", bound.AccountLogin, bound.AccountType)
	}
	// The other pending source must be untouched.
	other, _, _ := env.store.GitSourceByGithubAppID(ctx, 55)
	if other.Installed() {
		t.Error("bound the installation to the wrong source")
	}
}

func TestGithubSetupRejectsAnUnownedInstallation(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	env.gh.InstallationsByApp = map[int64][]github.Installation{}

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/setup?installation_id=4242", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// The setup route is unauthenticated (GitHub calls it directly), so its error
// body must never carry internal detail — only the fixed, non-revealing
// message, and never the raw installation id or anything that looks like
// driver/SQL text.
func TestGithubSetupUnownedInstallationBodyHasNoInternalDetail(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	env.gh.InstallationsByApp = map[int64][]github.Installation{}

	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec,
		httptest.NewRequest("GET", "/github/setup?installation_id=4242", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got := strings.TrimSpace(rec.Body.String())
	want := "no connected GitHub App owns this installation"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if strings.Contains(got, "4242") {
		t.Error("body must not echo the installation id back to an unauthenticated caller")
	}
	for _, term := range []string{"sql", "SQL", "database", "driver"} {
		if strings.Contains(got, term) {
			t.Errorf("body leaked internal detail: contains %q", term)
		}
	}
}

func TestGithubConnectOffersPersonalAndOrg(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	page := body(t, env.get(t, "/github/connect"))
	if !strings.Contains(page, `name="owner"`) {
		t.Error("connect page must ask where to create the App")
	}
	if !strings.Contains(page, `value="org"`) {
		t.Error("connect page must offer an organization")
	}
}

func TestGithubConnectOrgTargetsTheOrgAppForm(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	page := body(t, env.get(t, "/github/connect?owner=org&org=acme-corp"))
	if !strings.Contains(page, "github.com/organizations/acme-corp/settings/apps/new") {
		t.Errorf("org flow must post to the org App form; page did not contain it")
	}
}

func TestGithubConnectRejectsAMalformedOrg(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	res := env.get(t, "/github/connect?owner=org&org=not/an/org")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}
