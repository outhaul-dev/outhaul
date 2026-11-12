package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

var limitDefaults = core.Limits{MilliCPUs: 1000, Memory: 1 << 30, Pids: 512}

// seedLimitApp creates a project holding one app, so roles and project scope
// behave as they do for a real app.
func seedLimitApp(t *testing.T, e *testEnv, name string) core.App {
	t.Helper()
	ctx := context.Background()
	p, err := e.store.CreateProject(ctx, core.Project{Name: name})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app, err := e.store.CreateApp(ctx, core.App{
		Name: name, ProjectID: p.ID, RepoURL: "https://example.com/" + name + ".git",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func (e *testEnv) postLimits(t *testing.T, token string, appID int64, form url.Values) *http.Response {
	t.Helper()
	return e.postAs(t, token, fmt.Sprintf("/apps/%d/limits", appID), form)
}

func TestAppLimitsRouteNeedsADefault(t *testing.T) {
	e := newTestEnv(t)
	app := seedLimitApp(t, e, "shop")
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	resp := e.postLimits(t, admin, app.ID, url.Values{"limit_cpus": {"1"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no default limit", resp.StatusCode)
	}
}

func TestAppLimitsRouteIsSystemAdminOnly(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")

	member := e.signInAs(t, "member", core.RoleProjectMember, app.ProjectID)
	if resp := e.postLimits(t, member, app.ID, url.Values{"limit_cpus": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project member status = %d, want 403", resp.StatusCode)
	}

	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	if resp := e.postLimits(t, admin, app.ID, url.Values{"limit_cpus": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("system admin status = %d, want 303", resp.StatusCode)
	}
}

func TestAppLimitsRouteValidates(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	cases := []struct{ field, value, want string }{
		{"limit_cpus", "50%", "CPUs"},
		{"limit_memory", "5m", "6m"},
		{"limit_pids", "0", "processes"},
		{"limit_cpus", strconv.Itoa(runtime.NumCPU() + 1), "CPUs"},
	}
	for _, tc := range cases {
		resp := e.postLimits(t, admin, app.ID, url.Values{tc.field: {tc.value}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s=%q status = %d, want 400", tc.field, tc.value, resp.StatusCode)
			continue
		}
		if b := body(t, resp); !strings.Contains(b, tc.want) {
			t.Errorf("%s=%q body %q does not name %q", tc.field, tc.value, b, tc.want)
		}
	}
	if got, _ := e.store.GetApp(context.Background(), app.ID); got.Limits != (core.Limits{}) {
		t.Errorf("a refused value was stored: %+v", got.Limits)
	}
}

func TestAppLimitsRouteStoresAndClears(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	resp := e.postLimits(t, admin, app.ID, url.Values{
		"limit_cpus":   {"0.5"},
		"limit_memory": {"1g"},
		"limit_pids":   {"512"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	want := core.Limits{MilliCPUs: 500, Memory: 1 << 30, Pids: 512}
	if got, _ := e.store.GetApp(context.Background(), app.ID); got.Limits != want {
		t.Errorf("stored limits = %+v, want %+v", got.Limits, want)
	}

	// All three empty means "server default", stored as zeros.
	resp = e.postLimits(t, admin, app.ID, url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear status = %d, want 303", resp.StatusCode)
	}
	if got, _ := e.store.GetApp(context.Background(), app.ID); got.Limits != (core.Limits{}) {
		t.Errorf("cleared limits = %+v, want zero", got.Limits)
	}
}

// A value may be above or below the default; nothing is clamped to it.
func TestAppLimitsMayBeAboveOrBelowTheDefault(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	resp := e.postLimits(t, admin, app.ID, url.Values{
		"limit_cpus":   {"0.5"},
		"limit_memory": {"2g"},
		"limit_pids":   {"1024"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	want := core.Limits{MilliCPUs: 500, Memory: 2 << 30, Pids: 1024}
	if got, _ := e.store.GetApp(context.Background(), app.ID); got.Limits != want {
		t.Errorf("stored limits = %+v, want %+v (not clamped to the default)", got.Limits, want)
	}
}

func TestPreviewLimitsCannotBeChanged(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	parent := seedLimitApp(t, e, "shop")
	child, err := e.store.CreateApp(context.Background(), core.App{
		Name: "shop-pr-1", RepoURL: parent.RepoURL, ParentID: parent.ID, PRNumber: 1, Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("CreateApp preview: %v", err)
	}
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	resp := e.postLimits(t, admin, child.ID, url.Values{"limit_cpus": {"1"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if b := body(t, resp); !strings.Contains(b, "a preview uses its parent's limits") {
		t.Errorf("body %q does not explain that a preview uses its parent's limits", b)
	}
	if got, _ := e.store.GetApp(context.Background(), child.ID); got.Limits != (core.Limits{}) {
		t.Errorf("preview limits were stored: %+v", got.Limits)
	}
}

func TestLimitsPanelOnlyWithADefault(t *testing.T) {
	e := newTestEnv(t)
	app := seedLimitApp(t, e, "shop")
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	if b := body(t, e.getAs(t, admin, fmt.Sprintf("/apps/%d", app.ID))); strings.Contains(b, `id="limits"`) {
		t.Error("the Limits panel is shown with no default limit set")
	}
}

func TestLimitsPanelShowsTheValueInForce(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")
	if err := e.store.SetAppLimits(context.Background(), app.ID, core.Limits{Memory: 2 << 30}); err != nil {
		t.Fatalf("SetAppLimits: %v", err)
	}
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	b := body(t, e.getAs(t, admin, fmt.Sprintf("/apps/%d", app.ID)))
	for _, want := range []string{
		`id="limits"`,
		"1 CPU",
		"server default",   // CPU
		"2.0 GiB",          // memory, set for this app
		"set for this app", // memory's source
		"512",              // processes, the default
	} {
		if !strings.Contains(b, want) {
			t.Errorf("app page missing %q", want)
		}
	}
}

func TestLimitsPanelFormIsSystemAdminOnly(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	app := seedLimitApp(t, e, "shop")
	viewer := e.signInAs(t, "viewer", core.RoleProjectViewer, app.ProjectID)

	b := body(t, e.getAs(t, viewer, fmt.Sprintf("/apps/%d", app.ID)))
	if !strings.Contains(b, `id="limits"`) || !strings.Contains(b, "1 CPU") {
		t.Error("a viewer does not see the values in force")
	}
	if strings.Contains(b, `name="limit_cpus"`) {
		t.Error("a viewer is shown the limits form")
	}

	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	b = body(t, e.getAs(t, admin, fmt.Sprintf("/apps/%d", app.ID)))
	if !strings.Contains(b, `name="limit_cpus"`) {
		t.Error("a system admin has no limits form")
	}
}

func TestPreviewPanelShowsTheParentsLimits(t *testing.T) {
	e := newTestEnv(t)
	e.srv.SetDefaultLimits(limitDefaults)
	parent := seedLimitApp(t, e, "shop")
	if err := e.store.SetAppLimits(context.Background(), parent.ID, core.Limits{Memory: 2 << 30}); err != nil {
		t.Fatalf("SetAppLimits: %v", err)
	}
	child, err := e.store.CreateApp(context.Background(), core.App{
		Name: "shop-pr-1", RepoURL: parent.RepoURL, ParentID: parent.ID, PRNumber: 1, Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("CreateApp preview: %v", err)
	}
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)

	b := body(t, e.getAs(t, admin, fmt.Sprintf("/apps/%d", child.ID)))
	if !strings.Contains(b, "2.0 GiB") || !strings.Contains(b, "set for the parent app") {
		t.Errorf("preview page does not show the parent's limits:\n%s", b)
	}
	if strings.Contains(b, `name="limit_cpus"`) {
		t.Error("a preview is shown the limits form")
	}
}
