package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/catalog"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/previewmgr"
)

const noAccessCopy = "You don't have access to Outhaul yet"

// signInAs creates an account with the given role on the given projects and a
// session for it, directly through the store so no password is hashed.
func (e *testEnv) signInAs(t *testing.T, name string, role core.Role, projects ...int64) (sessionToken string) {
	t.Helper()
	ctx := context.Background()
	u, err := e.store.CreatePasswordAccount(ctx, name, role)
	if err != nil {
		t.Fatalf("CreatePasswordAccount(%s): %v", name, err)
	}
	for _, p := range projects {
		if err := e.store.AddProjectMember(ctx, p, u.ID); err != nil {
			t.Fatalf("AddProjectMember: %v", err)
		}
	}
	sessionToken = NewToken()
	if err := e.store.CreateSession(ctx, core.Session{
		Token: sessionToken, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sessionToken
}

// accountClient keeps no cookie jar, so a request carries only the session it
// is given, whoever else e.client is signed in as.
var accountClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (e *testEnv) requestAs(t *testing.T, token, method, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// getAs sends a GET with this session and no other; "" means signed out.
func (e *testEnv) getAs(t *testing.T, token, path string) *http.Response {
	t.Helper()
	return e.requestAs(t, token, http.MethodGet, path, nil)
}

// postAs sends a form POST with this session and no other; "" means signed out.
func (e *testEnv) postAs(t *testing.T, token, path string, form url.Values) *http.Response {
	t.Helper()
	return e.requestAs(t, token, http.MethodPost, path, form)
}

// seededProject is a project holding one of everything a route can name.
type seededProject struct {
	Project    core.Project
	App        core.App
	Database   core.Database
	Deployment core.Deployment
	Domain     core.Domain
	Volume     core.Volume
	Attachment core.Attachment
	Backup     core.Backup
}

// seedProject creates a project and its objects. Their names start with name,
// so a test can seed several.
func seedProject(t *testing.T, e *testEnv, name string) seededProject {
	t.Helper()
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seedProject(%s): %v", name, err)
		}
	}
	var (
		s   seededProject
		err error
	)
	s.Project, err = e.store.CreateProject(ctx, core.Project{Name: name})
	check(err)
	s.App, err = e.store.CreateApp(ctx, core.App{
		Name: name + "-app", ProjectID: s.Project.ID, RepoURL: "https://example.com/" + name + ".git",
	})
	check(err)
	s.Database, err = e.store.CreateDatabase(ctx, core.Database{
		Name: name + "-db", ProjectID: s.Project.ID, Engine: core.EnginePostgres, Image: "postgres:17",
		Username: name, DBName: name, Password: "pw123",
	})
	check(err)
	s.Deployment, err = e.store.CreateDeployment(ctx, s.App.ID)
	check(err)
	s.Domain, err = e.store.AddDomain(ctx, core.Domain{AppID: s.App.ID, Host: name + ".example.com", Port: 8080})
	check(err)
	s.Volume, err = e.store.AddVolume(ctx, s.App.ID, "/data")
	check(err)
	s.Attachment, err = e.store.AttachDatabase(ctx, s.App.ID, s.Database.ID, "DATABASE_URL")
	check(err)
	dest := seedDestination(t, e, name+"-dest")
	s.Backup, err = e.store.CreateBackup(ctx, core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: s.Database.ID, DestinationID: dest.ID, Schedule: "0 3 * * *",
	})
	check(err)
	return s
}

// target turns a route pattern into a method and a path naming this project's
// objects. A wildcard it doesn't know is fatal, so a new route can't slip past
// a walk of the table.
func (s seededProject) target(t *testing.T, pattern string) (method, path string) {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("pattern %q has no method", pattern)
	}
	path = strings.TrimSuffix(path, "{$}")
	ids := map[string]string{
		"projects":    itoa(s.Project.ID),
		"apps":        itoa(s.App.ID),
		"databases":   itoa(s.Database.ID),
		"deployments": itoa(s.Deployment.ID),
		"backups":     itoa(s.Backup.ID),
		"templates":   "any-template",
		"settings":    "1",
		"users":       "1",
		"account":     "1",
	}
	if id, ok := ids[strings.Split(path, "/")[1]]; ok {
		path = strings.ReplaceAll(path, "{id}", id)
	}
	path = strings.NewReplacer(
		"{domainID}", itoa(s.Domain.ID),
		"{volumeID}", itoa(s.Volume.ID),
		"{attachmentID}", itoa(s.Attachment.ID),
		"{previewID}", itoa(s.App.ID),
		"{userID}", "1",
	).Replace(path)
	if strings.Contains(path, "{") {
		t.Fatalf("pattern %q: no test value for a wildcard in %q", pattern, path)
	}
	return method, path
}

// form names this project for the routes that take it from the form.
func (s seededProject) form() url.Values {
	return url.Values{
		"project_id":  {itoa(s.Project.ID)},
		"target_kind": {core.BackupTargetDatabase},
		"target_id":   {itoa(s.Database.ID)},
	}
}

func TestEveryRouteStatesAnAccessLevel(t *testing.T) {
	for _, rt := range newTestEnv(t).srv.routes() {
		if rt.level == levelUnset {
			t.Errorf("%s has no access level", rt.pattern)
		}
		if rt.level.projectScoped() != (rt.project != nil) {
			t.Errorf("%s: project resolver doesn't match its level", rt.pattern)
		}
		if rt.handler == nil {
			t.Errorf("%s has no handler", rt.pattern)
		}
	}
}

// A route made public by mistake is the worst way the table can fail, and no walk
// can catch it, because a public route has nobody to refuse. So this list is kept
// by hand: a new public route means adding a line here on purpose.
func TestPublicRoutesAreExactlyThese(t *testing.T) {
	want := []string{
		"GET /account/setup",
		"GET /auth/sso/callback",
		"GET /auth/sso/start",
		"GET /ca.pem",
		"GET /deployments",
		"GET /gate/check",
		"GET /gate/sign-out",
		"GET /gate/start",
		"GET /github/callback",
		"GET /github/setup",
		"GET /healthz",
		"GET /login",
		"GET /setup",
		"GET /static/",
		"POST /account/setup",
		"POST /gate/sign-in",
		"POST /gate/sign-out",
		"POST /login",
		"POST /logout",
		"POST /setup",
		"POST /webhooks/app/{token}",
		"POST /webhooks/github",
	}
	var got []string
	for _, rt := range newTestEnv(t).srv.routes() {
		if rt.level == levelPublic {
			got = append(got, rt.pattern)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if slices.Equal(got, want) {
		return
	}
	var added, removed []string
	for _, p := range got {
		if !slices.Contains(want, p) {
			added = append(added, p)
		}
	}
	for _, p := range want {
		if !slices.Contains(got, p) {
			removed = append(removed, p)
		}
	}
	t.Errorf("the public routes changed\nadded, so anyone can use them without signing in: %q\nremoved: %q", added, removed)
}

// Walks every route in the table, substituting ids of seeded objects: a project
// with an app, a database, a deployment, a domain, a volume, an attachment and a
// backup. It only asserts refusals, so no handler with side effects runs.
func TestEveryRouteRefusesTheWrongPeople(t *testing.T) {
	e := newTestEnv(t)
	other := seedProject(t, e, "bravo") // first, so the two projects' ids differ
	s := seedProject(t, e, "alpha")
	on := s.Project.ID

	projectScoped := []accessLevel{levelViewer, levelMember, levelProjectAdmin}
	aboveAccount := append([]accessLevel{levelAnyRole, levelMemberRole, levelProjectAdminRole, levelSystemAdmin}, projectScoped...)

	tests := []struct {
		who    string
		token  string
		levels []accessLevel
		status int
		check  func(t *testing.T, pattern string, resp *http.Response)
	}{
		{
			who: "signed out", levels: append([]accessLevel{levelAccount}, aboveAccount...), status: http.StatusSeeOther,
			check: func(t *testing.T, pattern string, resp *http.Response) {
				if loc := resp.Header.Get("Location"); loc != "/login" {
					t.Errorf("%s: Location = %q, want /login", pattern, loc)
				}
			},
		},
		{who: "no role, on the project", token: e.signInAs(t, "none-on", core.RoleNone, on), levels: aboveAccount, status: http.StatusForbidden, check: wantNoAccessPage},
		{who: "no role, on no project", token: e.signInAs(t, "none-off", core.RoleNone), levels: aboveAccount, status: http.StatusForbidden, check: wantNoAccessPage},

		{who: "viewer on no project", token: e.signInAs(t, "viewer-off", core.RoleProjectViewer), levels: projectScoped, status: http.StatusNotFound},
		{who: "member on no project", token: e.signInAs(t, "member-off", core.RoleProjectMember), levels: projectScoped, status: http.StatusNotFound},
		{who: "project admin on no project", token: e.signInAs(t, "admin-off", core.RoleProjectAdmin), levels: projectScoped, status: http.StatusNotFound},
		{who: "project admin on another project", token: e.signInAs(t, "admin-other", core.RoleProjectAdmin, other.Project.ID), levels: projectScoped, status: http.StatusNotFound},

		{
			who: "viewer on the project", token: e.signInAs(t, "viewer-on", core.RoleProjectViewer, on), status: http.StatusForbidden, check: wantPlainRefusal,
			levels: []accessLevel{levelMemberRole, levelProjectAdminRole, levelSystemAdmin, levelMember, levelProjectAdmin},
		},
		{
			who: "member on the project", token: e.signInAs(t, "member-on", core.RoleProjectMember, on), status: http.StatusForbidden, check: wantPlainRefusal,
			levels: []accessLevel{levelProjectAdminRole, levelSystemAdmin, levelProjectAdmin},
		},
		{
			who: "project admin on the project", token: e.signInAs(t, "admin-on", core.RoleProjectAdmin, on), status: http.StatusForbidden, check: wantPlainRefusal,
			levels: []accessLevel{levelSystemAdmin},
		},
	}

	routes := e.srv.routes()
	for _, tc := range tests {
		t.Run(tc.who, func(t *testing.T) {
			walked := 0
			for _, rt := range routes {
				if !slices.Contains(tc.levels, rt.level) {
					continue
				}
				walked++
				method, path := s.target(t, rt.pattern)
				resp := e.requestAs(t, tc.token, method, path, s.form())
				if resp.StatusCode != tc.status {
					t.Errorf("%s %s = %d, want %d", method, path, resp.StatusCode, tc.status)
				}
				if tc.check != nil {
					tc.check(t, rt.pattern, resp)
				}
				resp.Body.Close()
			}
			if walked == 0 {
				t.Fatal("no route has any of these levels")
			}
		})
	}
}

func wantNoAccessPage(t *testing.T, pattern string, resp *http.Response) {
	t.Helper()
	if !strings.Contains(body(t, resp), noAccessCopy) {
		t.Errorf("%s: not the no-access page", pattern)
	}
}

// A role too low for an action is told so plainly; the no-access page is for
// people with no role at all.
func wantPlainRefusal(t *testing.T, pattern string, resp *http.Response) {
	t.Helper()
	if strings.Contains(body(t, resp), noAccessCopy) {
		t.Errorf("%s: a role that's too low got the no-access page", pattern)
	}
}

// The refusal walk would pass if the guard refused everyone, so this walks the
// other way: each level lets in the lowest role it admits. The requests are ones
// the handlers turn down on validation, or that change nothing that matters here.
func TestEachRolePassesItsLevel(t *testing.T) {
	e := newTestEnv(t)
	s := seedProject(t, e, "alpha")
	on := s.Project.ID
	viewer := e.signInAs(t, "viewer", core.RoleProjectViewer, on)
	member := e.signInAs(t, "member", core.RoleProjectMember, on)
	admin := e.signInAs(t, "project-admin", core.RoleProjectAdmin, on)
	templates, err := catalog.All()
	if err != nil || len(templates) == 0 {
		t.Fatalf("catalog.All: %v, %d templates", err, len(templates))
	}
	badName := url.Values{"name": {"NOT A NAME"}, "project_id": {itoa(on)}}

	tests := []struct {
		token  string
		method string
		path   string
		form   url.Values
		want   int
	}{
		{viewer, http.MethodGet, "/", nil, http.StatusOK},
		{viewer, http.MethodGet, "/projects", nil, http.StatusOK},
		{viewer, http.MethodGet, "/apps", nil, http.StatusOK},
		{viewer, http.MethodGet, "/databases", nil, http.StatusOK},
		{viewer, http.MethodGet, "/domains", nil, http.StatusOK},
		{viewer, http.MethodGet, "/projects/" + itoa(on), nil, http.StatusOK},
		{viewer, http.MethodGet, "/apps/" + itoa(s.App.ID), nil, http.StatusOK},
		{viewer, http.MethodGet, "/databases/" + itoa(s.Database.ID), nil, http.StatusOK},
		{viewer, http.MethodGet, "/deployments/" + itoa(s.Deployment.ID), nil, http.StatusOK},

		{member, http.MethodGet, "/templates", nil, http.StatusOK},
		{member, http.MethodPost, "/apps", badName, http.StatusBadRequest},
		{member, http.MethodPost, "/templates/" + templates[0].ID, badName, http.StatusBadRequest},
		{member, http.MethodPost, "/projects/" + itoa(on) + "/settings", badName, http.StatusBadRequest},
		{member, http.MethodPost, "/apps/" + itoa(s.App.ID) + "/env", url.Values{"key": {"not a key"}}, http.StatusBadRequest},
		{member, http.MethodPost, "/databases/" + itoa(s.Database.ID) + "/settings", url.Values{"external_port": {"not a port"}}, http.StatusBadRequest},
		{member, http.MethodPost, "/deployments/" + itoa(s.Deployment.ID) + "/cancel", nil, http.StatusSeeOther},
		{member, http.MethodPost, "/backups", s.form(), http.StatusBadRequest}, // no destination
		{member, http.MethodPost, "/backups/" + itoa(s.Backup.ID) + "/toggle", nil, http.StatusSeeOther},
		{member, http.MethodGet, "/backups/" + itoa(s.Backup.ID) + "/restore", nil, http.StatusOK},

		{admin, http.MethodPost, "/projects", badName, http.StatusBadRequest},
		{admin, http.MethodPost, "/projects/" + itoa(on) + "/delete", nil, http.StatusConflict}, // it has apps
	}
	for _, tc := range tests {
		resp := e.requestAs(t, tc.token, tc.method, tc.path, tc.form)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.path, resp.StatusCode, tc.want, body(t, resp))
		}
		resp.Body.Close()
	}
}

// resolveVia runs a route's resolver behind a real mux, so a resolver reading a
// wildcard its pattern doesn't declare finds nothing, as it would in production.
func resolveVia(rt route, method, path string, form url.Values) (projectID int64, found bool, err error) {
	mux := http.NewServeMux()
	mux.HandleFunc(rt.pattern, func(_ http.ResponseWriter, r *http.Request) {
		projectID, found, err = rt.project(r)
	})
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	return projectID, found, err
}

// Being refused by membership proves nothing if the resolver found the wrong
// project, so every project-scoped route must find the one its object is in.
func TestEveryProjectRouteFindsItsProject(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	alpha := seedProject(t, e, "alpha")
	// Put bravo's ids out of step with each other, so a resolver that looks up
	// the wrong kind of object finds nothing, or something in the wrong project.
	if _, err := e.store.CreateApp(ctx, core.App{Name: "filler", ProjectID: alpha.Project.ID}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := e.store.CreateDeployment(ctx, alpha.App.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.CreateBackup(ctx, core.Backup{
			TargetKind: core.BackupTargetDatabase, TargetID: alpha.Database.ID, DestinationID: alpha.Backup.DestinationID,
			Schedule: "0 3 * * *", Prefix: NewToken(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	bravo := seedProject(t, e, "bravo")

	walked := 0
	for _, s := range []seededProject{alpha, bravo} {
		for _, rt := range e.srv.routes() {
			if rt.project == nil {
				continue
			}
			walked++
			method, path := s.target(t, rt.pattern)
			got, found, err := resolveVia(rt, method, path, s.form())
			if err != nil || !found || got != s.Project.ID {
				t.Errorf("%s %s: resolver = (%d, %v, %v), want project %d", method, path, got, found, err, s.Project.ID)
			}
		}
	}
	if walked == 0 {
		t.Fatal("no project-scoped routes")
	}
}

// An unknown id, wherever it comes from, is "not found" rather than an error.
func TestProjectResolversReportNotFound(t *testing.T) {
	e := newTestEnv(t)
	s := seedProject(t, e, "alpha")
	dest := seedDestination(t, e, "dest")
	orphan, err := e.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetApp, TargetID: 9999, DestinationID: dest.ID, Schedule: "0 3 * * *",
	})
	if err != nil {
		t.Fatal(err)
	}

	pathRoutes := map[string]projectResolver{
		"GET /projects/{id}":    e.srv.projectOfPath,
		"GET /apps/{id}":        e.srv.projectOfApp,
		"GET /databases/{id}":   e.srv.projectOfDatabase,
		"GET /deployments/{id}": e.srv.projectOfDeployment,
		"POST /backups/{id}/x":  e.srv.projectOfBackup,
	}
	for pattern, resolver := range pathRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		for _, bad := range []string{"abc", "0", "-1"} {
			p := strings.ReplaceAll(path, "{id}", bad)
			if _, found, err := resolveVia(route{pattern: pattern, project: resolver}, method, p, nil); found || err != nil {
				t.Errorf("%s %s: found=%v err=%v, want not found", method, p, found, err)
			}
		}
	}
	for pattern, resolver := range pathRoutes {
		if pattern == "GET /projects/{id}" {
			continue // no lookup: membership of a project that doesn't exist is false
		}
		method, path, _ := strings.Cut(pattern, " ")
		p := strings.ReplaceAll(path, "{id}", "9999")
		if _, found, err := resolveVia(route{pattern: pattern, project: resolver}, method, p, nil); found || err != nil {
			t.Errorf("%s %s: found=%v err=%v, want not found", method, p, found, err)
		}
	}

	backup := route{pattern: "POST /backups/{id}/run", project: e.srv.projectOfBackup}
	if _, found, err := resolveVia(backup, http.MethodPost, "/backups/"+itoa(orphan.ID)+"/run", nil); found || err != nil {
		t.Errorf("backup with a missing target: found=%v err=%v, want not found", found, err)
	}

	form := route{pattern: "POST /backups", project: e.srv.projectOfBackupForm}
	for name, values := range map[string]url.Values{
		"unknown kind":   {"target_kind": {"volume"}, "target_id": {itoa(s.Database.ID)}},
		"no kind":        {"target_id": {itoa(s.Database.ID)}},
		"non-numeric id": {"target_kind": {core.BackupTargetDatabase}, "target_id": {"abc"}},
		"no id":          {"target_kind": {core.BackupTargetDatabase}},
		"missing db":     {"target_kind": {core.BackupTargetDatabase}, "target_id": {"9999"}},
		"missing app":    {"target_kind": {core.BackupTargetApp}, "target_id": {"9999"}},
	} {
		if _, found, err := resolveVia(form, http.MethodPost, "/backups", values); found || err != nil {
			t.Errorf("%s: found=%v err=%v, want not found", name, found, err)
		}
	}
}

// projectOfForm has to read the form the way handleCreateApp and
// handleDeployTemplate do, so the project that's checked is the one that's used.
func TestProjectOfFormReadsTheFormLikeTheHandlers(t *testing.T) {
	e := newTestEnv(t)
	rt := route{pattern: "POST /apps", project: e.srv.projectOfForm}
	tests := []struct {
		value     string
		want      int64
		wantFound bool
	}{
		{"", core.DefaultProjectID, true},
		{"   ", core.DefaultProjectID, true},
		{"7", 7, true},
		{" 7 ", 7, true},
		{"abc", 0, false},
		{"7x", 0, false},
		{"0", 0, false},
		{"-7", 0, false},
	}
	for _, tc := range tests {
		got, found, err := resolveVia(rt, http.MethodPost, "/apps", url.Values{"project_id": {tc.value}})
		if err != nil || found != tc.wantFound || got != tc.want {
			t.Errorf("project_id=%q: (%d, %v, %v), want (%d, %v, nil)", tc.value, got, found, err, tc.want, tc.wantFound)
		}
	}
	// No field at all is the same as an empty one.
	if got, found, err := resolveVia(rt, http.MethodPost, "/apps", url.Values{}); err != nil || !found || got != core.DefaultProjectID {
		t.Errorf("no project_id: (%d, %v, %v), want the default project", got, found, err)
	}
}

// An app and a database can share a numeric id across projects, so a backup's
// project depends on its target kind.
func TestBackupResolversFollowTheTargetKind(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	appProject, err := e.store.CreateProject(ctx, core.Project{Name: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	dbProject, err := e.store.CreateProject(ctx, core.Project{Name: "dbs"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := e.store.CreateApp(ctx, core.App{Name: "same-id-app", ProjectID: appProject.ID})
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.store.CreateDatabase(ctx, core.Database{
		Name: "same-id-db", ProjectID: dbProject.ID, Engine: core.EnginePostgres, Image: "postgres:17", Username: "u", DBName: "d", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != db.ID {
		t.Fatalf("the test needs equal ids, got app %d and database %d", app.ID, db.ID)
	}
	dest := seedDestination(t, e, "dest")

	for kind, want := range map[string]int64{core.BackupTargetApp: appProject.ID, core.BackupTargetDatabase: dbProject.ID} {
		b, err := e.store.CreateBackup(ctx, core.Backup{TargetKind: kind, TargetID: app.ID, DestinationID: dest.ID, Schedule: "0 3 * * *"})
		if err != nil {
			t.Fatal(err)
		}
		got, found, err := resolveVia(route{pattern: "POST /backups/{id}/run", project: e.srv.projectOfBackup},
			http.MethodPost, "/backups/"+itoa(b.ID)+"/run", nil)
		if err != nil || !found || got != want {
			t.Errorf("projectOfBackup, %s target: (%d, %v, %v), want project %d", kind, got, found, err, want)
		}
		got, found, err = resolveVia(route{pattern: "POST /backups", project: e.srv.projectOfBackupForm},
			http.MethodPost, "/backups", url.Values{"target_kind": {kind}, "target_id": {itoa(app.ID)}})
		if err != nil || !found || got != want {
			t.Errorf("projectOfBackupForm, %s target: (%d, %v, %v), want project %d", kind, got, found, err, want)
		}
	}
}

func TestGuardAnswers500WhenTheStoreFails(t *testing.T) {
	e := newTestEnv(t)
	s := seedProject(t, e, "alpha")
	token := e.signInAs(t, "mina", core.RoleProjectMember, s.Project.ID)

	ran := false
	h := e.srv.guard(route{
		pattern: "POST /x",
		level:   levelMember,
		project: func(*http.Request) (int64, bool, error) { return 0, false, errors.New("disk on fire") },
		handler: func(http.ResponseWriter, *http.Request) { ran = true },
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ran {
		t.Error("the handler ran after the resolver failed")
	}

	// A lookup that fails isn't "not found": the resolvers pass the error up.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, resolver := range map[string]projectResolver{
		"projectOfApp":        e.srv.projectOfApp,
		"projectOfDatabase":   e.srv.projectOfDatabase,
		"projectOfDeployment": e.srv.projectOfDeployment,
		"projectOfBackup":     e.srv.projectOfBackup,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(cancelled)
		req.SetPathValue("id", "1")
		if _, _, err := resolver(req); err == nil {
			t.Errorf("%s: no error from a failed lookup", name)
		}
	}
	req = httptest.NewRequest(http.MethodPost, "/backups",
		strings.NewReader(url.Values{"target_kind": {core.BackupTargetApp}, "target_id": {"1"}}.Encode())).WithContext(cancelled)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, _, err := e.srv.projectOfBackupForm(req); err == nil {
		t.Error("projectOfBackupForm: no error from a failed lookup")
	}
}

// rawDB is a second connection to the test database, for states the store's own
// methods won't create. Foreign keys are off on it.
func (e *testEnv) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(e.dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The session loads and the account doesn't, because its row can't be read. That
// stands in for any store failure. A cancelled context can't test this: it fails
// the session lookup first, which is a different branch.
//
// A redirect here would loop: /login sends a valid session back to /.
func TestGuardAnswers500WhenTheAccountCantBeLoaded(t *testing.T) {
	e := newTestEnv(t)
	token := e.signInAs(t, "mina", core.RoleProjectMember)
	if _, err := e.rawDB(t).Exec(`UPDATE users SET created_at = 'not a time' WHERE username = 'mina'`); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	ran := false
	h := e.srv.guard(route{
		pattern: "GET /x",
		level:   levelAnyRole,
		handler: func(http.ResponseWriter, *http.Request) { ran = true },
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d (Location %q), want 500", rec.Code, rec.Header().Get("Location"))
	}
	if ran {
		t.Error("the handler ran without an account")
	}
	if !strings.Contains(logged.String(), "GET /x") || !strings.Contains(logged.String(), "not a time") {
		t.Errorf("the failure wasn't logged with its route and cause: %q", logged.String())
	}
	if strings.Contains(rec.Body.String(), "not a time") {
		t.Errorf("the store's error reached the browser: %q", rec.Body.String())
	}

	// And through the real table, where the loop would start.
	resp := e.getAs(t, token, "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET / = %d (Location %q), want 500", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// An account that's really gone is someone signed out, not a failure. The foreign
// key stops this happening today, so it takes a connection with foreign keys off.
func TestGuardSendsASessionWithNoAccountToLogin(t *testing.T) {
	e := newTestEnv(t)
	token := e.signInAs(t, "mina", core.RoleProjectMember)
	if _, err := e.rawDB(t).Exec(`DELETE FROM users WHERE username = 'mina'`); err != nil {
		t.Fatal(err)
	}

	resp := e.getAs(t, token, "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("GET / = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRoleChangeAppliesToTheNextRequest(t *testing.T) {
	e := newTestEnv(t)
	s := seedProject(t, e, "alpha")
	token := e.signInAs(t, "mina", core.RoleProjectMember, s.Project.ID)
	path := "/apps/" + itoa(s.App.ID)

	resp := e.getAs(t, token, path)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member: status = %d, want 200", resp.StatusCode)
	}

	u, err := e.store.GetUserByName(context.Background(), "mina")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetUserRole(context.Background(), u.ID, core.RoleNone); err != nil {
		t.Fatal(err)
	}

	resp = e.getAs(t, token, path)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("after the role was removed: status = %d, want 403", resp.StatusCode)
	}
}

// The guard checks the project named by {id}. These routes also name a child
// object, which must belong to that app and not to one in another project.
func TestChildObjectsMustBelongToTheApp(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	a := seedProject(t, e, "alpha")
	b := seedProject(t, e, "bravo")
	preview, err := e.store.CreateApp(ctx, core.App{
		Name: core.PreviewAppName(b.App.Name, 7), ProjectID: b.Project.ID, Source: core.SourceGithub, GithubRepo: "me/app",
		Branch: "feature", ParentID: b.App.ID, PRNumber: 7, Ephemeral: true, PreviewStatus: core.PreviewReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The real manager, because that's where the parent check lives. It refuses
	// before it touches any of its other dependencies.
	e.srv.previews = previewmgr.New(e.store, nil, nil, nil, nil, nil, "")
	member := e.signInAs(t, "mina", core.RoleProjectMember, a.Project.ID)
	onA := "/apps/" + itoa(a.App.ID)

	for _, tc := range []struct {
		path string
		form url.Values
		want int // deleting a row that isn't the app's is a no-op redirect
	}{
		{onA + "/domains/" + itoa(b.Domain.ID) + "/delete", nil, http.StatusSeeOther},
		{onA + "/domains/" + itoa(b.Domain.ID), url.Values{"host": {"stolen.example.com"}, "port": {"8080"}}, http.StatusNotFound},
		{onA + "/volumes/" + itoa(b.Volume.ID) + "/delete", nil, http.StatusSeeOther},
		{onA + "/volumes/" + itoa(b.Volume.ID), url.Values{"mount_path": {"/stolen"}}, http.StatusNotFound},
		{onA + "/attachments/" + itoa(b.Attachment.ID) + "/delete", nil, http.StatusSeeOther},
		{onA + "/previews/" + itoa(preview.ID) + "/destroy", nil, http.StatusNotFound},
		{onA + "/previews/99999/destroy", nil, http.StatusNotFound},
		{onA + "/previews/" + itoa(b.App.ID) + "/destroy", nil, http.StatusNotFound}, // an app, not a preview
		{onA + "/attachments", url.Values{"database_id": {itoa(b.Database.ID)}, "env_var": {"STOLEN_URL"}}, http.StatusNotFound},
	} {
		resp := e.postAs(t, member, tc.path, tc.form)
		got := body(t, resp)
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
		if strings.Contains(got, b.Database.Name) {
			t.Errorf("POST %s: the reply names the other project's database %q: %s", tc.path, b.Database.Name, got)
		}
	}

	domain, err := e.store.GetDomain(ctx, b.App.ID, b.Domain.ID)
	if err != nil {
		t.Errorf("B's domain: %v", err)
	} else if domain.Host != b.Domain.Host {
		t.Errorf("B's domain host = %q, want %q", domain.Host, b.Domain.Host)
	}
	volume, err := e.store.GetVolume(ctx, b.App.ID, b.Volume.ID)
	if err != nil {
		t.Errorf("B's volume: %v", err)
	} else if volume.MountPath != b.Volume.MountPath {
		t.Errorf("B's volume mount = %q, want %q", volume.MountPath, b.Volume.MountPath)
	}
	attachments, err := e.store.ListAttachments(ctx, b.App.ID)
	if err != nil || len(attachments) != 1 {
		t.Errorf("B's attachments = %v (%v), want its one", attachments, err)
	}
	if _, err := e.store.GetApp(ctx, preview.ID); err != nil {
		t.Errorf("B's preview: %v", err)
	}
	// A's app didn't pick up B's database either.
	attachments, err = e.store.ListAttachments(ctx, a.App.ID)
	if err != nil || len(attachments) != 1 || attachments[0].ID != a.Attachment.ID {
		t.Errorf("A's attachments = %v (%v), want only its own", attachments, err)
	}
}

func TestNoRolePersonSeesNoAccessPage(t *testing.T) {
	e := newTestEnv(t)
	s := seedProject(t, e, "alpha")
	token := e.signInAs(t, "nora", core.RoleNone, s.Project.ID)

	resp := e.getAs(t, token, "/")
	page := body(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	for _, want := range []string{noAccessCopy, "Ask a system admin to give you a role.", "You can still use apps on protected routes.", `action="/logout"`, "Sign out", `href="/account"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not contain %q:\n%s", want, page)
		}
	}
	for _, leak := range []string{s.Project.Name, s.App.Name, s.Database.Name, "sidebar"} {
		if strings.Contains(page, leak) {
			t.Errorf("page contains %q:\n%s", leak, page)
		}
	}

	// Signing out still works: it's a public route.
	resp = e.postAs(t, token, "/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("sign out = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// /account is for changing a password, so an SSO account gets no link to it.
func TestNoAccessPageLinksAccountOnlyForPasswords(t *testing.T) {
	e := newTestEnv(t)
	for signIn, want := range map[string]bool{core.SignInPassword: true, core.SignInSSO: false} {
		req := withUser(httptest.NewRequest(http.MethodGet, "/", nil), core.User{SignIn: signIn, Role: core.RoleNone})
		rec := httptest.NewRecorder()
		e.srv.render(rec, req, http.StatusForbidden, "noaccess", map[string]any{"HideChrome": true})
		if got := strings.Contains(rec.Body.String(), `href="/account"`); got != want {
			t.Errorf("%s account: link to /account = %v, want %v", signIn, got, want)
		}
	}
}

func TestSystemAdminKeepsEveryRoute(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	s := seedProject(t, e, "alpha")
	for _, path := range []string{"/settings", "/containers", "/apps/" + itoa(s.App.ID)} {
		resp := e.get(t, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}
