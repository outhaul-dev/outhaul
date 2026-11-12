package server

import (
	"context"
	"database/sql"
	"errors"
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

// The form's switch is on, a single-container app's own row is protected, and
// the proxy was reconciled so the gate's host is mapped.
func TestCreateAppProtectsItsRow(t *testing.T) {
	e, px := newProtectionEnv(t)
	e.srv.protectNew = true
	form := appForm("web", "web.test")
	form.Set("protected_offered", "1")
	form.Set("protected", "on")
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}
	if px.calls != 1 {
		t.Errorf("proxy reconciled %d times, want the gate host mapped once", px.calls)
	}
	page := body(t, e.get(t, "/apps"))
	if !strings.Contains(page, `name="protected_offered"`) || !strings.Contains(page, `name="protected" checked`) {
		t.Error("the new-app form doesn't offer the sign-in switch, on")
	}
}

// A request that never saw the form is protected too; the form's own "off"
// opens the row.
func TestCreateAppWithoutTheFormsFieldIsProtected(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	r := e.postForm(t, "/apps", appForm("web", "web.test")) // neither field, as a script sends it
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}
}

func TestCreateAppFormSwitchOffOpensTheRow(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	form := appForm("web", "web.test")
	form.Set("protected_offered", "1") // the switch off: offered, not named
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Protected {
		t.Errorf("rows = %+v, want one open", rows)
	}
}

// A compose app's first row, given in the form, is protected too; an
// internal-only compose app has no row to protect and is created as before.
func TestCreateComposeAppProtectsItsFirstRow(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	form := composeForm("shop")
	form.Set("protected_offered", "1")
	form.Set("protected", "on")
	r := e.postForm(t, "/apps", form)
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "shop")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || !rows[0].Protected {
		t.Errorf("rows = %+v, want one protected", rows)
	}

	internal := composeForm("box")
	internal.Del("domain")
	internal.Del("compose_service")
	internal.Del("compose_port")
	r = e.postForm(t, "/apps", internal)
	if r.StatusCode >= 400 {
		t.Fatalf("internal-only compose app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	if _, err := e.store.GetAppByName(context.Background(), "box"); err != nil {
		t.Errorf("internal-only compose app was not created: %v", err)
	}
}

// When the proxy can't be reconciled the app and its row are removed again,
// so nothing is left that claims sign-in it doesn't have.
func TestCreateAppRefusesWhenTheGateHostFails(t *testing.T) {
	e, px := newProtectionEnv(t)
	e.srv.protectNew = true
	px.err = errors.New("docker unreachable")

	r := e.postForm(t, "/apps", appForm("web", "web.test"))
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "docker unreachable") {
		t.Fatalf("create app: status %d, want a refusal naming the cause", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a failed gate host: %v", err)
	}
}

// An app that still serves an old open route on the host is refused, not
// redeployed from here: the new row would be reachable openly until it does.
func TestCreateAppRefusesAnotherAppsOldRoute(t *testing.T) {
	e, _ := newProtectionEnv(t)
	e.srv.protectNew = true
	leftoverHooks(t, e, []core.Domain{{Host: "shop.test", Port: deployAppPort}}, nil)

	r := e.postForm(t, "/apps", appForm("web", "shop.test"))
	if r.StatusCode < 400 || !strings.Contains(body(t, r), "hooks") {
		t.Fatalf("create app: status %d, want a refusal naming the app that must redeploy", r.StatusCode)
	}
	if _, err := e.store.GetAppByName(context.Background(), "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("app survived a refused host: %v", err)
	}
}

// With the setting off the form offers no switch and the row is open.
func TestCreateAppWithoutTheSettingLeavesTheRowOpen(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	r := e.postForm(t, "/apps", appForm("web", "web.test"))
	if r.StatusCode >= 400 {
		t.Fatalf("create app: status %d, body %s", r.StatusCode, body(t, r))
	}
	r.Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")
	rows, _ := e.store.ListDomains(context.Background(), app.ID)
	if len(rows) != 1 || rows[0].Protected {
		t.Errorf("rows = %+v, want one open", rows)
	}
	page := body(t, e.get(t, "/apps"))
	if strings.Contains(page, `name="protected"`) {
		t.Error("the new-app form offers a sign-in switch with the setting off")
	}
}
