package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
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

func TestCreatePushAppRejectsRepoURL(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	form := url.Values{
		"name": {"pushy2"}, "domain": {"pushy2.example.com"},
		"source": {"push"}, "kind": {"nixpacks"}, "branch": {"main"},
		"repo_url": {"https://github.com/o/r.git"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if _, err := env.store.GetAppByName(context.Background(), "pushy2"); err == nil {
		t.Fatal("push app with a repo_url should have been rejected")
	}
}
