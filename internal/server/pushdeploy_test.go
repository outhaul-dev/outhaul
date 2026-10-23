package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCreatePushApp(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	form := url.Values{
		"name": {"pushy"}, "domain": {"pushy.example.com"},
		"source": {"push"}, "kind": {"nixpacks"}, "branch": {"main"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	app, err := env.store.GetAppByName(context.Background(), "pushy")
	if err != nil {
		t.Fatal(err)
	}
	if app.Source != core.SourcePush {
		t.Errorf("source = %q, want push", app.Source)
	}
	if app.RepoURL != "" {
		t.Errorf("push app should have empty RepoURL, got %q", app.RepoURL)
	}
}

// The Git URL box is hidden, not removed, for a push app: a URL typed before
// switching the source to Push is still submitted. It must not refuse the
// create — the reopened form would keep Push and the hidden URL, and refuse
// every resubmit.
func TestCreatePushAppIgnoresHiddenGitURL(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	form := url.Values{
		"name": {"pushy"}, "domain": {"pushy.example.com"}, "source": {"push"},
		"repo_url": {"https://github.com/o/r.git"}, "kind": {"nixpacks"}, "branch": {"main"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body)
	}
	app, err := env.store.GetAppByName(context.Background(), "pushy")
	if err != nil {
		t.Fatal(err)
	}
	if app.Source != core.SourcePush || app.RepoURL != "" {
		t.Errorf("source = %q, repo = %q; want push with no repository URL", app.Source, app.RepoURL)
	}
}

func TestPushAppDetailShowsRemote(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "pushed", Domain: "pushed.example.com",
		Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/apps/"+strconv.FormatInt(app.ID, 10), nil)
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body)
	}
	page := rec.Body.String()
	if !strings.Contains(page, "Deploy with git push") {
		t.Error("push app page missing the git-push setup hint")
	}
	if !strings.Contains(page, "git remote add outhaul ssh://git@") {
		t.Error("push app page missing the ssh remote snippet")
	}
}
