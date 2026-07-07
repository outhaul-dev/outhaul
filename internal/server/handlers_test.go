package server

import (
	"context"
	"net/http"
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

func TestAppDetailRendersAllKinds(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	cases := []core.App{
		{Name: "nix", RepoURL: "https://example.com/r.git", Domain: "nix.example.com", Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "a"},
		{Name: "dock", RepoURL: "https://example.com/r.git", Domain: "dock.example.com", Source: core.SourcePublic, Branch: "main", Kind: core.KindDockerfile, DockerfilePath: "Dockerfile", WebhookSecret: "b"},
		{Name: "comp", Source: core.SourcePublic, RepoURL: "https://example.com/r.git", Branch: "main", Kind: core.KindCompose, ComposePath: "docker-compose.yml", WebhookSecret: "c"},
		{Name: "tmpl", Source: core.SourceTemplate, TemplateID: "ghost", Kind: core.KindCompose, ComposeRaw: "services: {}", WebhookSecret: "d"},
		{Name: "pushapp", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks, Domain: "push.example.com", WebhookSecret: "e"},
	}
	for _, a := range cases {
		app, err := env.store.CreateApp(ctx, a)
		if err != nil {
			t.Fatalf("create %s: %v", a.Name, err)
		}
		resp := env.get(t, "/apps/"+itoa(app.ID))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", a.Name, resp.StatusCode)
		}
		page := body(t, resp)
		if !strings.Contains(page, `data-tab="overview"`) {
			t.Fatalf("%s: tabs missing", a.Name)
		}
		// Source editor is present for repo apps, absent for template apps.
		hasSourceEditor := strings.Contains(page, `/source"`)
		if a.Source == core.SourceTemplate && hasSourceEditor {
			t.Fatalf("%s: template app must not show source editor", a.Name)
		}
		if a.Source != core.SourceTemplate && !hasSourceEditor {
			t.Fatalf("%s: repo app must show source editor", a.Name)
		}
	}
}
