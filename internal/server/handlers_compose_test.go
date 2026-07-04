package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/docker"
)

// composeForm builds a valid create-app submission for a compose app.
func composeForm(name string) url.Values {
	return url.Values{
		"name": {name}, "kind": {"compose"},
		"source": {"public"}, "repo_url": {"https://github.com/o/" + name + ".git"},
		"branch": {"main"},
		"domain": {name + ".example.com"}, "compose_service": {"web"}, "compose_port": {"3000"},
	}
}

func TestCreateComposeApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	resp := e.postForm(t, "/apps", composeForm("shop"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	app, err := e.store.GetAppByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if app.Kind != core.KindCompose {
		t.Errorf("Kind = %q, want compose", app.Kind)
	}
	if app.ComposePath != "docker-compose.yml" {
		t.Errorf("ComposePath = %q, want the docker-compose.yml default", app.ComposePath)
	}
	if app.ComposeService != "web" || app.ComposePort != 3000 {
		t.Errorf("exposure = %q:%d, want web:3000", app.ComposeService, app.ComposePort)
	}
}

func TestCreateComposeAppWithoutDomain(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	form := composeForm("internal")
	form.Set("domain", "")
	form.Set("compose_service", "")
	form.Set("compose_port", "")
	resp := e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d, want 303 (domain is optional for compose); body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	app, _ := e.store.GetAppByName(context.Background(), "internal")
	if app.Domain != "" || app.ComposeService != "" || app.ComposePort != 0 {
		t.Errorf("internal-only stack should have no exposure: %+v", app)
	}
}

func TestCreateComposeAppValidation(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	// Domain set but no service named.
	form := composeForm("shop")
	form.Set("compose_service", "")
	resp := e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing service = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "compose service") {
		t.Error("expected the expose-service error")
	}

	// Bad port.
	form = composeForm("shop")
	form.Set("compose_port", "99999")
	resp = e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad port = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Path escaping the repo.
	form = composeForm("shop")
	form.Set("compose_path", "../outside.yml")
	resp = e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaping path = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "relative path") {
		t.Error("expected the compose-path error")
	}

	// Nixpacks apps still require a domain.
	resp = e.postForm(t, "/apps", url.Values{
		"name": {"web"}, "kind": {"nixpacks"}, "source": {"public"},
		"repo_url": {"https://github.com/o/web.git"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("nixpacks without domain = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestComposeAppSettingsUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/settings", url.Values{
		"branch": {"release"}, "auto_deploy": {"on"},
		"watch_paths":     {"deploy/**\n\n  src/** \n"},
		"domain":          {"shop.example.com"},
		"compose_path":    {"deploy/compose.yml"},
		"compose_service": {"frontend"},
		"compose_port":    {"8081"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	got, _ := e.store.GetApp(context.Background(), app.ID)
	if got.Branch != "release" || !got.AutoDeploy {
		t.Errorf("branch settings not updated: %+v", got)
	}
	if len(got.WatchPaths) != 2 || got.WatchPaths[0] != "deploy/**" || got.WatchPaths[1] != "src/**" {
		t.Errorf("watch paths = %v, want trimmed two entries", got.WatchPaths)
	}
	if got.ComposePath != "deploy/compose.yml" || got.ComposeService != "frontend" || got.ComposePort != 8081 {
		t.Errorf("compose settings not updated: %+v", got)
	}
}

func TestComposeLifecycleUsesRunner(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/stop", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/restart", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restart = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()

	if n := len(e.compose.CallsFor("stop")); n != 1 {
		t.Errorf("compose stop calls = %d, want 1", n)
	}
	calls := e.compose.CallsFor("restart")
	if len(calls) != 1 || calls[0].Project != "slipway-shop" {
		t.Errorf("compose restart calls = %+v, want one for slipway-shop", calls)
	}
	if len(e.runtime.stopped) != 0 || len(e.runtime.started) != 0 {
		t.Error("compose lifecycle must not touch single containers")
	}
}

func TestDeleteComposeAppDownsStack(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")

	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()

	downs := e.compose.CallsFor("down")
	if len(downs) != 1 || downs[0].Project != "slipway-shop" {
		t.Errorf("compose down calls = %+v, want one for slipway-shop", downs)
	}
	if _, err := e.store.GetApp(context.Background(), app.ID); err == nil {
		t.Error("app row should be deleted")
	}
}

func TestComposeAppDetailShowsStack(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")

	e.runtime.stack = []docker.Container{
		{ID: "c1", Name: "slipway-shop-web-1", State: "running",
			Labels: map[string]string{"com.docker.compose.service": "web"}},
		{ID: "c2", Name: "slipway-shop-db-1", State: "exited",
			Labels: map[string]string{"com.docker.compose.service": "db"}},
	}

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "Compose") {
		t.Error("app detail missing the Compose badge")
	}
	if !strings.Contains(page, "1/2 running") {
		t.Error("app detail should summarize stack state as 1/2 running")
	}
	for _, svc := range []string{"web", "db"} {
		if !strings.Contains(page, ">"+svc+"<") {
			t.Errorf("services table missing %q", svc)
		}
	}
	if !strings.Contains(page, "Watch paths") {
		t.Error("settings form missing the watch-paths field")
	}
}
