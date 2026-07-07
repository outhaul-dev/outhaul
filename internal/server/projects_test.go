package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestProjectsListRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/projects")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /projects = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestProjectsListShowsCards(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	// Two apps in the default project; the card should count them.
	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	e.postForm(t, "/apps", appForm("api", "api.example.com")).Body.Close()

	page := body(t, e.get(t, "/projects"))
	if !strings.Contains(page, "Projects") {
		t.Error("projects page missing heading")
	}
	if !strings.Contains(page, "default") {
		t.Error("projects page missing the default project card")
	}
	if !strings.Contains(page, "2 apps") {
		t.Error("default project card should count its 2 apps")
	}
}

func TestCreateProjectAndDetail(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	resp := e.postForm(t, "/projects", url.Values{"name": {"acme"}, "description": {"Acme Corp"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/projects" {
		t.Fatalf("create project = %d -> %q, want 303 -> /projects", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	projects, err := e.store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var acme core.Project
	for _, p := range projects {
		if p.Name == "acme" {
			acme = p
		}
	}
	if acme.ID == 0 {
		t.Fatal("acme project not created")
	}

	page := body(t, e.get(t, "/projects/"+itoa(acme.ID)))
	if !strings.Contains(page, "acme") || !strings.Contains(page, "Acme Corp") {
		t.Error("project detail should show name and description")
	}
	if !strings.Contains(page, "No apps in this project yet") {
		t.Error("empty project should show the empty state")
	}
}

func TestCreateProjectValidatesName(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	resp := e.postForm(t, "/projects", url.Values{"name": {"Bad Name!"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid project name = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "lowercase letters") {
		t.Error("expected the name-validation error on the page")
	}

	resp = e.postForm(t, "/projects", url.Values{"name": {"default"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate project name = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateAppIntoProject(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/projects", url.Values{"name": {"acme"}}).Body.Close()
	projects, _ := e.store.ListProjects(context.Background())
	var acme core.Project
	for _, p := range projects {
		if p.Name == "acme" {
			acme = p
		}
	}

	form := appForm("web", "web.example.com")
	form.Set("project_id", itoa(acme.ID))
	form.Set("return", "/projects/"+itoa(acme.ID))
	resp := e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/projects/"+itoa(acme.ID) {
		t.Fatalf("create app = %d -> %q, want 303 back to the project", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if app.ProjectID != acme.ID {
		t.Errorf("app ProjectID = %d, want %d", app.ProjectID, acme.ID)
	}

	page := body(t, e.get(t, "/projects/"+itoa(acme.ID)))
	if !strings.Contains(page, "/apps/"+itoa(app.ID)) {
		t.Error("project detail should list the app it contains")
	}
}

func TestCreateAppDefaultsToDefaultProject(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if app.ProjectID != core.DefaultProjectID {
		t.Errorf("app ProjectID = %d, want default %d", app.ProjectID, core.DefaultProjectID)
	}
}

func TestCreateAppRejectsUnknownProject(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	form := appForm("web", "web.example.com")
	form.Set("project_id", "999")
	resp := e.postForm(t, "/apps", form)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown project = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "Choose a project") {
		t.Error("expected the choose-a-project error on the page")
	}
}

func TestProjectSettingsUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/projects", url.Values{"name": {"acme"}}).Body.Close()
	projects, _ := e.store.ListProjects(context.Background())
	var acme core.Project
	for _, p := range projects {
		if p.Name == "acme" {
			acme = p
		}
	}

	resp := e.postForm(t, "/projects/"+itoa(acme.ID)+"/settings",
		url.Values{"name": {"acme-prod"}, "description": {"renamed"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()

	got, err := e.store.GetProject(context.Background(), acme.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "acme-prod" || got.Description != "renamed" {
		t.Errorf("after settings update = %+v", got)
	}
}

func TestDeleteProjectGuardedThenAllowed(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/projects", url.Values{"name": {"acme"}}).Body.Close()
	projects, _ := e.store.ListProjects(context.Background())
	var acme core.Project
	for _, p := range projects {
		if p.Name == "acme" {
			acme = p
		}
	}
	form := appForm("web", "web.example.com")
	form.Set("project_id", itoa(acme.ID))
	e.postForm(t, "/apps", form).Body.Close()

	resp := e.postForm(t, "/projects/"+itoa(acme.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete non-empty project = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "still has 1 app") {
		t.Error("expected the not-empty error naming the app count")
	}
	if _, err := e.store.GetProject(context.Background(), acme.ID); err != nil {
		t.Fatal("project should survive a refused delete")
	}

	app, _ := e.store.GetAppByName(context.Background(), "web")
	e.postForm(t, "/apps/"+itoa(app.ID)+"/delete", url.Values{}).Body.Close()

	resp = e.postForm(t, "/projects/"+itoa(acme.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/projects" {
		t.Fatalf("delete empty project = %d -> %q, want 303 -> /projects", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
	if _, err := e.store.GetProject(context.Background(), acme.ID); err == nil {
		t.Error("project should be gone after delete")
	}
}

func TestAppsListShowsProjectColumn(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	page := body(t, e.get(t, "/apps"))
	if !strings.Contains(page, "Project") {
		t.Error("apps list missing the Project column")
	}
	if !strings.Contains(page, "/projects/"+itoa(core.DefaultProjectID)) {
		t.Error("apps list should link the app's project")
	}
}

func TestAppDetailBreadcrumbNamesProject(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	e.postForm(t, "/apps", appForm("web", "web.example.com")).Body.Close()
	app, _ := e.store.GetAppByName(context.Background(), "web")

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "/projects/"+itoa(core.DefaultProjectID)) {
		t.Error("app detail breadcrumb should link the app's project")
	}
}

func TestProjectsPageUsesProjectWording(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/projects")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /projects = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if strings.Contains(strings.ToLower(page), "workspace") {
		t.Error(`projects page should say "project", not "workspace"`)
	}
}

// projectByName returns the project with the given name. Projects are created
// via the HTTP form (which returns no ID), so tests look them up by name.
func projectByName(t *testing.T, e *testEnv, name string) core.Project {
	t.Helper()
	projects, err := e.store.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range projects {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("project %q not found", name)
	return core.Project{}
}

func TestProjectPageHasCreateModals(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/projects", url.Values{"name": {"shop"}}).Body.Close()
	shop := projectByName(t, e, "shop")

	resp := e.get(t, "/projects/"+itoa(shop.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET project = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, `id="db-dialog"`) {
		t.Error("project page should contain the database create dialog")
	}
	if !strings.Contains(page, "action-bar") {
		t.Error("project page should have the action bar")
	}
}

func TestAppFormIsAWizard(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/projects", url.Values{"name": {"shop"}}).Body.Close()
	shop := projectByName(t, e, "shop")

	page := body(t, e.get(t, "/projects/"+itoa(shop.ID)))
	if strings.Count(page, `data-step`) < 3 {
		t.Errorf("app form should have 3 wizard steps, markup:\n%s", page)
	}
	if !strings.Contains(page, `name="project_id"`) {
		t.Error("app form must still submit a project_id")
	}
	// On a project page the project is implied — a hidden input, not a <select>.
	if strings.Contains(page, `<select name="project_id"`) {
		t.Error("project page app form should not show a project dropdown (InProject)")
	}
}

func TestDatabaseCreateErrorReopensDialog(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.postForm(t, "/projects", url.Values{"name": {"shop"}}).Body.Close()
	shop := projectByName(t, e, "shop")

	// An invalid database name is rejected and re-renders the project page.
	resp := e.postForm(t, "/projects/"+itoa(shop.ID)+"/databases", url.Values{
		"name": {"Bad Name"}, "engine": {"postgres"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad db create = %d, want 400", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "data-reopen") {
		t.Error("errored database dialog should be flagged to reopen")
	}
	if !strings.Contains(page, `value="Bad Name"`) {
		t.Error("database dialog should preserve the entered name on error")
	}
}

func TestAppCreateErrorReopensDialog(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	// An invalid app name is rejected and re-renders the apps page; the
	// create-app dialog must be flagged to reopen so the error is visible.
	resp := e.postForm(t, "/apps", appForm("Bad Name", "x.example.com"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad app create = %d, want 400", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "data-reopen") {
		t.Error("errored app dialog should be flagged to reopen")
	}
}
