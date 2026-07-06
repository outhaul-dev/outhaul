package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestAddDomainToNixpacksApp(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	form := url.Values{"host_kind": {"custom"}, "host": {"alias.test"}, "path": {"/api"}, "internal_path": {"/"}, "tls": {"on"}}
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("add domain: status %d", resp.StatusCode)
	}
	got, _ := e.store.ListDomains(context.Background(), app.ID)
	var found *core.Domain
	for i := range got {
		if got[i].Host == "alias.test" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("alias.test not stored: %+v", got)
	}
	if found.Service != "" || found.Port != 8080 || found.Path != "/api" || found.InternalPath != "/" || !found.TLS {
		t.Errorf("nixpacks domain stored wrong: %+v", *found)
	}
}

func TestAddDomainToComposeAppRequiresService(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})

	bad := url.Values{"host_kind": {"custom"}, "host": {"shop.test"}, "service": {""}, "port": {"3000"}}
	r1 := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", bad)
	r1.Body.Close()
	if r1.StatusCode < 400 {
		t.Errorf("compose domain without a service should fail, got %d", r1.StatusCode)
	}
	ok := url.Values{"host_kind": {"custom"}, "host": {"shop.test"}, "service": {"web"}, "port": {"3000"}}
	r2 := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", ok)
	r2.Body.Close()
	if r2.StatusCode >= 400 {
		t.Errorf("valid compose domain rejected: %d", r2.StatusCode)
	}
	got, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(got) != 1 || got[0].Service != "web" || got[0].Port != 3000 {
		t.Errorf("compose domain stored wrong: %+v", got)
	}
}

func TestAddDomainSslipGeneratesHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	form := url.Values{"host_kind": {"sslip"}}
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	r.Body.Close()
	if r.StatusCode >= 400 {
		t.Fatalf("sslip add failed: %d", r.StatusCode)
	}
	got, _ := e.store.ListDomains(context.Background(), app.ID)
	var hasSslip bool
	for _, d := range got {
		if strings.HasSuffix(d.Host, ".sslip.io") {
			hasSslip = true
		}
	}
	if !hasSslip {
		t.Errorf("expected a generated sslip.io host: %+v", got)
	}
}

func TestAddDomainInternalPathRequiresExternalPath(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	form := url.Values{"host_kind": {"custom"}, "host": {"x.test"}, "internal_path": {"/v1"}}
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	r.Body.Close()
	if r.StatusCode < 400 {
		t.Errorf("internal path without external path should be rejected, got %d", r.StatusCode)
	}
}

func TestUpdateAndDeleteDomain(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	d, _ := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "edit.test", Port: 8080, TLS: true})

	up := url.Values{"host_kind": {"custom"}, "host": {"edited.test"}, "tls": {""}}
	r1 := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(d.ID), up)
	r1.Body.Close()
	if r1.StatusCode >= 400 {
		t.Fatalf("update failed: %d", r1.StatusCode)
	}
	got, _ := e.store.GetDomain(context.Background(), app.ID, d.ID)
	if got.Host != "edited.test" || got.TLS {
		t.Errorf("update not applied: %+v", got)
	}

	r2 := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(d.ID)+"/delete", url.Values{})
	r2.Body.Close()
	if r2.StatusCode >= 400 {
		t.Fatalf("delete failed: %d", r2.StatusCode)
	}
	if list, _ := e.store.ListDomains(context.Background(), app.ID); len(list) != 1 { // only the seeded web.test remains
		t.Errorf("delete left %d rows, want 1", len(list))
	}
}
