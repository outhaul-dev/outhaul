package server

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
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

func TestDomainsPageTagsPreviewRows(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	// A normal app with a domain, plus an ephemeral preview child of it.
	parent, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	preview, _ := e.store.CreateApp(context.Background(), core.App{
		Name: "web-pr-7", RepoURL: "https://x/y.git", Domain: "pr7.web.test",
		Ephemeral: true, PRNumber: 7, ParentID: parent.ID,
	})
	_ = preview

	page := body(t, e.get(t, "/domains"))
	if !strings.Contains(page, "PR #7") {
		t.Errorf("/domains page missing preview tag %q", "PR #7")
	}
	// The preview row should link to the parent app, not the child.
	if !strings.Contains(page, "/apps/"+itoa(parent.ID)) {
		t.Errorf("/domains preview row should link to parent app /apps/%d", parent.ID)
	}
}

const ownAddressRefusal = "That is Outhaul's own address, so an app can't use it."

// refusedAsOwnAddress checks resp is a 400 saying the host is Outhaul's own.
func refusedAsOwnAddress(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	page := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest ||
		!(strings.Contains(page, ownAddressRefusal) || strings.Contains(page, html.EscapeString(ownAddressRefusal))) {
		t.Errorf("%s = %d, want 400 saying %q: %.300s", what, resp.StatusCode, ownAddressRefusal, page)
	}
}

// An app routed on the host of Outhaul's own public URL would share its
// origin and cookies, so no domain row may use it, whatever its case, port or
// path. A row already on it can still be deleted.
func TestAppCantUseOuthaulsOwnHost(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t) // public URL https://slip.example.com, server IP 203.0.113.7
	e.completeSetup(t)
	web, _ := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	shop, _ := e.store.CreateApp(ctx, core.App{Name: "shop", RepoURL: "https://x/y.git", Kind: core.KindCompose, ComposePath: "docker-compose.yml"})
	addTo := func(app core.App) string { return "/apps/" + itoa(app.ID) + "/domains" }

	for _, c := range []struct {
		what string
		app  core.App
		form url.Values
	}{
		{"add slip.example.com", web, url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}}},
		{"add SLIP.Example.com", web, url.Values{"host_kind": {"custom"}, "host": {" SLIP.Example.com "}}},
		{"add slip.example.com/x", web, url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}, "path": {"/x"}}},
		{"add it to a compose app", shop, url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}, "service": {"web"}, "port": {"3000"}}},
	} {
		refusedAsOwnAddress(t, c.what, e.postForm(t, addTo(c.app), c.form))
	}

	// A port on the public URL doesn't make its host another one.
	e.srv.publicURL = "https://Slip.Example.com:8443/"
	refusedAsOwnAddress(t, "add with a port on the public URL",
		e.postForm(t, addTo(web), url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}}))

	// A generated sslip.io host is checked too.
	e.srv.publicURL = "https://web.203.0.113.7.sslip.io"
	refusedAsOwnAddress(t, "add a generated sslip.io host", e.postForm(t, addTo(web), url.Values{"host_kind": {"sslip"}}))
	e.srv.publicURL = "https://slip.example.com"

	// Moving a row onto it is refused, and the row stays as it was.
	moving, _ := e.store.AddDomain(ctx, core.Domain{AppID: web.ID, Host: "edit.test", Port: 8080, TLS: true})
	refusedAsOwnAddress(t, "edit onto it", e.postForm(t, addTo(web)+"/"+itoa(moving.ID),
		url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}}))
	if got, _ := e.store.GetDomain(ctx, web.ID, moving.ID); got.Host != "edit.test" {
		t.Errorf("refused edit changed the row to %q", got.Host)
	}

	// A row already on it can't be edited in place, but can be deleted.
	old, err := e.store.AddDomain(ctx, core.Domain{AppID: web.ID, Host: "slip.example.com", Path: "/old", Port: 8080, TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	refusedAsOwnAddress(t, "edit a row already on it", e.postForm(t, addTo(web)+"/"+itoa(old.ID),
		url.Values{"host_kind": {"custom"}, "host": {"slip.example.com"}, "path": {"/other"}}))
	resp := e.postForm(t, addTo(web)+"/"+itoa(old.ID)+"/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("delete a row already on it = %d, want 303", resp.StatusCode)
	}

	// New apps can't start on it either.
	form := dockerfileForm("api")
	form.Set("domain", "slip.example.com")
	refusedAsOwnAddress(t, "create an app on it", e.postForm(t, "/apps", form))
	form = composeForm("stack")
	form.Set("domain", "Slip.Example.com")
	refusedAsOwnAddress(t, "create a compose app on it", e.postForm(t, "/apps", form))
	for _, name := range []string{"api", "stack"} {
		if _, err := e.store.GetAppByName(ctx, name); err == nil {
			t.Errorf("app %q was created", name)
		}
	}

	all, err := e.store.ListAllDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range all {
		if strings.EqualFold(d.Host, "slip.example.com") {
			t.Errorf("a row is on Outhaul's own host: %+v", d)
		}
	}

	// Any other host is still fine.
	resp = e.postForm(t, addTo(web), url.Values{"host_kind": {"custom"}, "host": {"app.example.com"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("add app.example.com = %d, want 303", resp.StatusCode)
	}
}
