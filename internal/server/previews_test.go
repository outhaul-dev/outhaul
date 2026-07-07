package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestPreviewsSectionRendersForGithubApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "Preview environments") {
		t.Fatalf("github app page missing Preview environments; body:\n%s", page)
	}
}

func TestPreviewsSectionHiddenForNonGithub(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://x/y.git", Kind: core.KindNixpacks, Domain: "web.test",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if strings.Contains(page, "Preview environments") {
		t.Fatalf("non-github app page should not show Preview environments; body:\n%s", page)
	}
}

func TestSavePreviewConfig(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	form := url.Values{
		"enabled":         {"on"},
		"base_domain":     {"preview.example.com"},
		"idle_ttl_days":   {"7"},
		"max_concurrent":  {"5"},
		"post_pr_comment": {"on"},
	}
	res := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", form)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save preview config status = %d, want 303", res.StatusCode)
	}
	cfg, err := e.store.GetPreviewConfig(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("GetPreviewConfig: %v", err)
	}
	if !cfg.Enabled || cfg.BaseDomain != "preview.example.com" {
		t.Fatalf("config not saved: %+v", cfg)
	}
}
