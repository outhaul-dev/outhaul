package server

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// The dialog's default host is the app's name under the suffix.
func TestAddDomainDefaultKind(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"default"}})
	if r.StatusCode >= 400 {
		t.Fatalf("default host: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	got, _ := e.store.ListDomains(context.Background(), app.ID)
	var hosts []string
	for _, d := range got {
		hosts = append(hosts, d.Host)
	}
	if !slices.Contains(hosts, "web.shared.demo.example.com") {
		t.Errorf("hosts = %q, want one on web.shared.demo.example.com", hosts)
	}

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `value="default"`) {
		t.Error("the dialog does not offer the default host")
	}
	if strings.Contains(page, `value="sslip"`) {
		t.Error("the dialog still offers a free sslip.io domain under a suffix")
	}
}

// Without a suffix the default kind has nothing to offer and says so.
func TestAddDomainDefaultKindNeedsASuffix(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"default"}})
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "suffix") {
		t.Errorf("default host with no suffix: status %d, want a refusal naming the suffix", r.StatusCode)
	}
}

// A single-container app with no domain takes its name under the suffix.
func TestCreateAppFillsTheDomainUnderASuffix(t *testing.T) {
	e := newTestEnv(t)
	e.srv.naming = core.Naming{ServerIP: "203.0.113.7", Suffix: "shared.demo.example.com"}
	e.completeSetup(t)
	r := e.postForm(t, "/apps", appForm("web", "")) // no domain: the form leaves it optional
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if app.Domain != "web.shared.demo.example.com" {
		t.Errorf("app domain = %q, want web.shared.demo.example.com", app.Domain)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Host != "web.shared.demo.example.com" {
		t.Errorf("rows = %+v, want one on web.shared.demo.example.com", rows)
	}
}

// With neither setting an install sees no change: the sslip option, a required
// domain, and no default option.
func TestNoSuffixKeepsTodaysNamesAndForms(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `value="sslip"`) {
		t.Error("the dialog no longer offers the free sslip.io domain")
	}
	if strings.Contains(page, `value="default"`) {
		t.Error("the dialog offers a default host without a suffix")
	}
	apps := body(t, e.get(t, "/apps"))
	if strings.Contains(apps, `data-default-domain="`) {
		t.Error("the app form mentions a default domain without a suffix")
	}
}
