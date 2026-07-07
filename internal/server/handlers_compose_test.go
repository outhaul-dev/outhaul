package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
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
	// apps.domain mirrors the first domain row (store.syncPrimaryDomainTx); it
	// is a read-only denormalization, not a second source of truth.
	if app.Domain != "shop.example.com" {
		t.Errorf("App.Domain = %q, want it mirrored from the seeded domain row", app.Domain)
	}
	domains, err := e.store.ListDomains(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 1 || domains[0].Host != "shop.example.com" ||
		domains[0].Service != "web" || domains[0].Port != 3000 {
		t.Errorf("domains = %+v, want the form's domain seeded as web:3000", domains)
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
	if app.Domain != "" {
		t.Errorf("internal-only stack should have no domain: %+v", app)
	}
	if ds, _ := e.store.ListDomains(context.Background(), app.ID); len(ds) != 0 {
		t.Errorf("internal-only stack should have no domain rows: %+v", ds)
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
		"watch_paths":  {"deploy/**\n\n  src/** \n"},
		"compose_path": {"deploy/compose.yml"},
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
	if got.ComposePath != "deploy/compose.yml" {
		t.Errorf("compose path not updated: %+v", got)
	}
	// Domains are managed by their own endpoints, untouched by settings saves.
	if ds, _ := e.store.ListDomains(context.Background(), app.ID); len(ds) != 1 {
		t.Errorf("settings save must not touch domains: %+v", ds)
	}
}

func TestComposeDomainAddAndRemove(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")

	// Add a second domain on another service.
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"api.example.com"}, "service": {"api"}, "port": {"8080"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add domain = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	domains, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(domains) != 2 || domains[0].Host != "api.example.com" {
		t.Fatalf("domains = %+v, want api.example.com added", domains)
	}

	// A duplicate host on the same app is rejected.
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"api.example.com"}, "service": {"other"}, "port": {"1234"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate domain = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Bad fields are rejected.
	for name, form := range map[string]url.Values{
		"missing service": {"host_kind": {"custom"}, "host": {"x.example.com"}, "port": {"80"}},
		"bad port":        {"host_kind": {"custom"}, "host": {"x.example.com"}, "service": {"web"}, "port": {"99999"}},
		"bad domain":      {"host_kind": {"custom"}, "host": {"not a host"}, "service": {"web"}, "port": {"80"}},
	} {
		resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Remove one; the other survives.
	resp = e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(domains[0].ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete domain = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()
	domains, _ = e.store.ListDomains(context.Background(), app.ID)
	if len(domains) != 1 || domains[0].Host != "shop.example.com" {
		t.Errorf("domains after delete = %+v, want just shop.example.com", domains)
	}
}

func TestDomainAddedToNixpacksApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.postForm(t, "/apps", url.Values{
		"name": {"web"}, "source": {"public"},
		"repo_url": {"https://github.com/o/web.git"}, "domain": {"web.example.com"},
	}).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")

	// service/port in the form are ignored for a single-container app.
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"extra.example.com"}, "service": {"web"}, "port": {"80"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add domain to nixpacks app = %d, want 303; body=%s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
	domains, _ := e.store.ListDomains(context.Background(), app.ID)
	var extra *core.Domain
	for i := range domains {
		if domains[i].Host == "extra.example.com" {
			extra = &domains[i]
		}
	}
	if extra == nil || extra.Service != "" || extra.Port != 8080 {
		t.Errorf("nixpacks domain not stored with forced service/port: %+v", domains)
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
	if len(calls) != 1 || calls[0].Project != "outhaul-shop" {
		t.Errorf("compose restart calls = %+v, want one for outhaul-shop", calls)
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
	if len(downs) != 1 || downs[0].Project != "outhaul-shop" {
		t.Errorf("compose down calls = %+v, want one for outhaul-shop", downs)
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
		{ID: "c1", Name: "outhaul-shop-web-1", State: "running",
			Labels: map[string]string{"com.docker.compose.service": "web"}},
		{ID: "c2", Name: "outhaul-shop-db-1", State: "exited",
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
	if !strings.Contains(page, "shop.example.com") {
		t.Error("domains panel missing the published domain")
	}
	if !strings.Contains(page, "/apps/"+itoa(app.ID)+"/domains") {
		t.Error("domains panel missing the add-domain form")
	}
	domains, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(domains) != 1 {
		t.Fatalf("domains = %+v", domains)
	}
	if !strings.Contains(page, "/domains/"+itoa(domains[0].ID)+"/delete") {
		t.Error("domains panel missing the remove button")
	}
}
