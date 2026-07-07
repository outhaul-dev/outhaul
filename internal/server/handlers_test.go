package server

import (
	"context"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestIsStatusTemplateFunc(t *testing.T) {
	fn, ok := templateFuncs()["isStatus"]
	if !ok {
		t.Fatal("isStatus template func not registered")
	}
	f := fn.(func(core.DeployStatus, string) bool)
	if !f(core.StatusRunning, "running") {
		t.Fatal(`running should match "running"`)
	}
	if f(core.StatusFailed, "running") {
		t.Fatal(`failed should not match "running"`)
	}
}

func TestAppDetailMarksNewestRunningLive(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	seedFinishedDeploy(t, env, app.ID, "outhaul/web:1")
	seedFinishedDeploy(t, env, app.ID, "outhaul/web:2")

	page := body(t, env.get(t, "/apps/"+itoa(app.ID)))
	if strings.Count(page, ">live<") != 1 {
		t.Fatalf("want exactly one live badge, page had %d", strings.Count(page, ">live<"))
	}
	if !strings.Contains(page, ">superseded<") {
		t.Fatal("older running deployment should read superseded")
	}
}

func TestAppDetailShowsSourceEditor(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	app, _ := env.store.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	page := body(t, env.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `action="/apps/`+itoa(app.ID)+`/source"`) {
		t.Fatal("source editor form missing")
	}
	if !strings.Contains(page, `action="/apps/`+itoa(app.ID)+`/kind"`) {
		t.Fatal("build-type editor form missing")
	}

	// A template app has no repo, so the editor must be absent.
	tmpl, _ := env.store.CreateApp(ctx, core.App{
		Name: "tapp", Source: core.SourceTemplate, TemplateID: "ghost",
		Kind: core.KindCompose, ComposeRaw: "services: {}", WebhookSecret: "w2",
	})
	tpage := body(t, env.get(t, "/apps/"+itoa(tmpl.ID)))
	if strings.Contains(tpage, `/source"`) {
		t.Fatal("template app must not show the source editor")
	}
}

func TestAppDetailHasTabs(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	page := body(t, env.get(t, "/apps/"+itoa(app.ID)))
	for _, tab := range []string{"overview", "deployments", "networking", "resources", "settings"} {
		if !strings.Contains(page, `data-tab="`+tab+`"`) {
			t.Fatalf("missing tab panel %q", tab)
		}
		if !strings.Contains(page, `href="#`+tab+`"`) {
			t.Fatalf("missing tab link %q", tab)
		}
	}
	if !strings.Contains(page, "Danger zone") {
		t.Fatal("danger zone missing")
	}
}
