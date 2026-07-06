package server

import (
	"context"
	"net/http"
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

func TestAddDomainSslipRequiresServerIP(t *testing.T) {
	e := newTestEnv(t)
	e.srv.serverIP = ""
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"sslip"}})
	r.Body.Close()
	if r.StatusCode < 400 {
		t.Errorf("sslip host with no server IP should be rejected, got %d", r.StatusCode)
	}
}

func TestAddDomainRejectsInjectionHost(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	form := url.Values{"host_kind": {"custom"}, "host": {"x`)||Host(`y"}}
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", form)
	r.Body.Close()
	if r.StatusCode < 400 {
		t.Errorf("injection host should be rejected, got %d", r.StatusCode)
	}
}

func TestUpdateDomainChangesServiceAndPort(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})
	d, _ := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "shop.test", Service: "web", Port: 3000, TLS: true})

	up := url.Values{"host_kind": {"custom"}, "host": {"shop.test"}, "service": {"api"}, "port": {"9000"}}
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(d.ID), up)
	r.Body.Close()
	if r.StatusCode >= 400 {
		t.Fatalf("update failed: %d", r.StatusCode)
	}
	got, _ := e.store.GetDomain(context.Background(), app.ID, d.ID)
	if got.Service != "api" || got.Port != 9000 {
		t.Errorf("service/port not updated: %+v", got)
	}
}

func TestUpdateNonexistentDomain404(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	form := url.Values{"host_kind": {"custom"}, "host": {"x.test"}}
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/9999", form)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("update of missing domain = %d, want 404", r.StatusCode)
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
	e.srv.tlsEnabled = true // ACME on: the TLS toggle is live, so an empty submit clears it
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

func TestUpdateDomainPreservesTLSWhenAcmeOff(t *testing.T) {
	e := newTestEnv(t)
	e.srv.tlsEnabled = false
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	d, _ := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "keep.test", Port: 8080, TLS: true})

	// Edit the host only; the disabled TLS checkbox sends nothing.
	up := url.Values{"host_kind": {"custom"}, "host": {"kept.test"}}
	e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(d.ID), up).Body.Close()

	got, _ := e.store.GetDomain(context.Background(), app.ID, d.ID)
	if got.Host != "kept.test" || !got.TLS {
		t.Errorf("TLS should be preserved when ACME is off: %+v", got)
	}
}

func TestAppPageShowsDomainWizard(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)

	// nixpacks app (seeds web.test) — wizard present, no compose service select.
	web, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	page := body(t, e.get(t, "/apps/"+itoa(web.ID)))
	for _, want := range []string{"id=\"domain-dialog\"", "openDomainDialog()", "web.test"} {
		if !strings.Contains(page, want) {
			t.Errorf("nixpacks app page missing %q", want)
		}
	}
	if strings.Contains(page, "id=\"domain-service\"") {
		t.Error("nixpacks app page should not show the compose service select")
	}

	// compose app with a domain — service select present in the wizard.
	shop, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})
	e.store.AddDomain(context.Background(), core.Domain{AppID: shop.ID, Host: "shop.test", Service: "web", Port: 3000})
	page = body(t, e.get(t, "/apps/"+itoa(shop.ID)))
	for _, want := range []string{"id=\"domain-dialog\"", "shop.test", "id=\"domain-service\""} {
		if !strings.Contains(page, want) {
			t.Errorf("compose app page missing %q", want)
		}
	}
}

func TestDomainsPageListsAcrossApps(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	// nixpacks app seeds web.test; compose app gets an explicit route.
	e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	shop, _ := e.store.CreateApp(context.Background(), core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})
	e.store.AddDomain(context.Background(), core.Domain{AppID: shop.ID, Host: "shop.test", Service: "api", Port: 8000, TLS: true})

	page := body(t, e.get(t, "/domains"))
	for _, want := range []string{"web.test", "shop.test", "api:8000"} {
		if !strings.Contains(page, want) {
			t.Errorf("/domains page missing %q", want)
		}
	}
}
