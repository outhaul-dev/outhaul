package server

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// httpsOnlyEnv is a testEnv whose server serves HTTPS only, with HTTPS
// automation available.
func httpsOnlyEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.srv.tlsEnabled = true
	e.srv.SetRouting(traefik.Routing{TLS: true, HTTPOff: true})
	e.completeSetup(t)
	return e
}

func TestHTTPSOnlyLocksTheHTTPSBox(t *testing.T) {
	e := httpsOnlyEnv(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "This server serves HTTPS only.") {
		t.Error("the domain form does not say the server serves HTTPS only")
	}
	m := regexp.MustCompile(`<input[^>]*id="domain-tls"[^>]*>`).FindString(page)
	if m == "" {
		t.Fatal("the domain form has no TLS checkbox")
	}
	if !strings.Contains(m, "checked") || !strings.Contains(m, "disabled") {
		t.Errorf("TLS checkbox = %q, want both checked and disabled", m)
	}
}

func TestHTTPSOnlyAddDomainStoresTLS(t *testing.T) {
	e := httpsOnlyEnv(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"alias.example.com"},
	})
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("add domain: status %d", resp.StatusCode)
	}
	domains, err := e.store.ListDomains(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found *core.Domain
	for i := range domains {
		if domains[i].Host == "alias.example.com" {
			found = &domains[i]
		}
	}
	if found == nil {
		t.Fatalf("alias.example.com not stored: %+v", domains)
	}
	if !found.TLS {
		t.Error("a domain added on an HTTPS-only server should store TLS true")
	}
}

func TestHTTPSOnlyEditKeepsTheStoredFlag(t *testing.T) {
	e := httpsOnlyEnv(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "plain.example.com", Port: 8080, TLS: false})
	if err != nil {
		t.Fatal(err)
	}
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains/"+itoa(d.ID), url.Values{
		"host_kind": {"custom"}, "host": {"plain.example.com"},
	})
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("edit domain: status %d", resp.StatusCode)
	}
	domains, err := e.store.ListDomains(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range domains {
		if got.ID == d.ID && got.TLS {
			t.Error("editing without a TLS field should keep the row's stored TLS off")
		}
	}
}

func TestHTTPSOnlyLinksAreHTTPS(t *testing.T) {
	e := httpsOnlyEnv(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "plain", RepoURL: "https://x/y.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddDomain(context.Background(), core.Domain{AppID: app.ID, Host: "plain.example.com", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `href="https://plain.example.com"`) {
		t.Error("the app page does not link the row over HTTPS")
	}
	if strings.Contains(page, `href="http://plain.example.com"`) {
		t.Error("the app page links the row over plain HTTP")
	}
	domains := body(t, e.get(t, "/domains"))
	if !strings.Contains(domains, `href="https://plain.example.com"`) {
		t.Error("the Domains page does not link the row over HTTPS")
	}
	if strings.Contains(domains, `href="http://plain.example.com"`) {
		t.Error("the Domains page links the row over plain HTTP")
	}
	apps := body(t, e.get(t, "/apps"))
	if !strings.Contains(apps, `href="https://plain.example.com"`) {
		t.Error("the Apps page does not link the row over HTTPS")
	}
	if strings.Contains(apps, `href="http://plain.example.com"`) {
		t.Error("the Apps page links the row over plain HTTP")
	}
	project := body(t, e.get(t, "/projects/1"))
	if !strings.Contains(project, `href="https://plain.example.com"`) {
		t.Error("the Project page does not link the row over HTTPS")
	}
	if strings.Contains(project, `href="http://plain.example.com"`) {
		t.Error("the Project page links the row over plain HTTP")
	}
}
