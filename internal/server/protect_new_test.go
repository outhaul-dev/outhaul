package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// A request that didn't come from the dialog (no protected_offered: a script,
// an old page) doesn't mean "open": the row is protected.
func TestNewDomainIsProtectedWithoutTheFormsField(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"shop.test"}})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	var added core.Domain
	for _, d := range rows {
		if d.Host == "shop.test" {
			added = d
		}
	}
	if !added.Protected {
		t.Errorf("row = %+v, want it protected", added)
	}
}

// A row that can't be protected is not created: with sign-in unavailable the
// save is refused with the reason, rather than storing an open row.
func TestNewDomainThatCannotBeProtectedIsRefused(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	e.srv.tlsEnabled = false
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"shop.test"}})
	b := body(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, b)
	}
	if !strings.Contains(b, protectNeedsHTTPS) {
		t.Errorf("refusal = %q, want it to say sign-in is unavailable", b)
	}
	for _, d := range domainsOf(t, e, app.ID) {
		if d.Host == "shop.test" {
			t.Errorf("row = %+v, want no row for an address that can't be protected", d)
		}
	}
}

// The dialog's switch starts on for a new row, and says how to open one.
func TestDomainDialogOffersSignInOnByDefault(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `id="domain-protected" checked`) {
		t.Error("the dialog's Require sign-in switch is not on for a new row")
	}
	if !strings.Contains(page, "turn this switch off on the host") {
		t.Error("the dialog doesn't say how to make an address public")
	}
}

// Turning the switch off in the dialog opens the row, as before.
func TestNewDomainOpenedByTheDialogsSwitch(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{
		"host_kind": {"custom"}, "host": {"shop.test"}, "protected_offered": {"1"},
	})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	added := storedRow(t, e, app.ID, "shop.test")
	if added.Protected {
		t.Errorf("row = %+v, want it open", added)
	}
}

// storedRow is the row for host that a save to app stored, failing when no
// such row exists, so a test can't pass on a row that was never added.
func storedRow(t *testing.T, e *testEnv, appID int64, host string) core.Domain {
	t.Helper()
	rows, err := e.store.ListDomains(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		if d.Host == host {
			return d
		}
	}
	t.Fatalf("row %s was not added", host)
	return core.Domain{}
}

// With the setting off nothing changes: a row is open, and the switch is the
// disabled, off one it was.
func TestProtectNewOffChangesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/domains", url.Values{"host_kind": {"custom"}, "host": {"shop.test"}})
	if r.StatusCode >= 400 {
		t.Fatalf("add domain: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	added := storedRow(t, e, app.ID, "shop.test")
	if added.Protected {
		t.Errorf("row = %+v, want it open with the setting off", added)
	}
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if strings.Contains(page, `id="domain-protected" checked`) {
		t.Error("the switch is on although the setting is off")
	}
}
