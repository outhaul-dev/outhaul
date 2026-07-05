package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
)

// seedDatabase inserts a database row directly (as handleCreateDatabase
// would), leaving container state to the test.
func seedDatabase(t *testing.T, e *testEnv, name, engine string) core.Database {
	t.Helper()
	d := core.Database{Name: name, Engine: engine, Image: "postgres:17", Password: "pw123"}
	if engine != core.EngineRedis {
		d.Username = name
		d.DBName = name
	}
	d, err := e.store.CreateDatabase(context.Background(), d)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	return d
}

func TestCreateDatabaseProvisionsAndRedirects(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	resp := e.postForm(t, "/projects/1/databases", url.Values{
		"name": {"shop-db"}, "engine": {"postgres"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/databases/") {
		t.Fatalf("redirect = %q, want the database page", loc)
	}
	id, ok := parseID(strings.TrimPrefix(loc, "/databases/"))
	if !ok {
		t.Fatalf("could not parse database ID from %q", loc)
	}

	d, err := e.store.GetDatabase(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != core.DBCreating || d.Engine != core.EnginePostgres || d.Image != "postgres:17" {
		t.Errorf("row = status %q engine %q image %q, want creating/postgres/default image", d.Status, d.Engine, d.Image)
	}
	if d.Username != "shop-db" || d.DBName != "shop-db" || len(d.Password) < 16 {
		t.Errorf("credentials = user %q db %q pw %d chars, want name-derived with a generated password", d.Username, d.DBName, len(d.Password))
	}
	if len(e.databases.provisioned) != 1 || e.databases.provisioned[0].ID != d.ID {
		t.Fatalf("provisioned = %+v, want exactly the new database", e.databases.provisioned)
	}
	if e.databases.provisioned[0].Password != d.Password {
		t.Error("manager received a different password than the one stored")
	}
}

func TestCreateDatabaseRedisHasNoUserDB(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	resp := e.postForm(t, "/projects/1/databases", url.Values{
		"name": {"cache"}, "engine": {"redis"}, "external_port": {"6380"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	d := e.databases.provisioned[0]
	if d.Username != "" || d.DBName != "" {
		t.Errorf("redis row has user %q db %q, want empty (password-only auth)", d.Username, d.DBName)
	}
	if d.ExtPort != 6380 || d.Image != "redis:7" {
		t.Errorf("row = port %d image %q, want 6380/redis:7", d.ExtPort, d.Image)
	}
}

func TestCreateDatabaseValidation(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"bad name", url.Values{"name": {"Bad_Name"}, "engine": {"postgres"}}, "Name"},
		{"reserved root", url.Values{"name": {"root"}, "engine": {"mysql"}}, "reserved"},
		{"unknown engine", url.Values{"name": {"shop"}, "engine": {"oracle"}}, "engine"},
		{"bad port", url.Values{"name": {"shop"}, "engine": {"postgres"}, "external_port": {"99999"}}, "port"},
	}
	for _, tc := range cases {
		resp := e.postForm(t, "/projects/1/databases", tc.form)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
		if got := body(t, resp); !strings.Contains(got, tc.want) {
			t.Errorf("%s: error page does not mention %q", tc.name, tc.want)
		}
	}
	if len(e.databases.provisioned) != 0 {
		t.Errorf("rejected forms still provisioned: %+v", e.databases.provisioned)
	}
}

func TestCreateDatabaseDuplicateName(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	seedDatabase(t, e, "shop", core.EnginePostgres)

	resp := e.postForm(t, "/projects/1/databases", url.Values{
		"name": {"shop"}, "engine": {"postgres"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "Could not create database") {
		t.Errorf("duplicate name error not surfaced, got page without it")
	}
}

func TestDatabasePageShowsConnectionURL(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)

	page := body(t, e.get(t, "/databases/"+itoa(d.ID)))
	if !strings.Contains(page, "postgres://shop:pw123@outhaul-db-shop:5432/shop") {
		t.Error("page missing the internal connection URL")
	}
	// The logs URL is assembled in JS from the data-id attribute.
	if !strings.Contains(page, `id="runtime-log" data-id="`+itoa(d.ID)+`"`) ||
		!strings.Contains(page, "'/databases/' + id + '/logs?tail='") {
		t.Error("page missing the runtime-logs panel")
	}
	if !strings.Contains(page, "/databases/"+itoa(d.ID)+"/settings") {
		t.Error("page missing the settings form")
	}
	if !strings.Contains(page, "SHOP_URL") {
		t.Error("page missing the project-env naming hint")
	}
}

func TestDatabaseStartStopDelete(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)

	if resp := e.postForm(t, "/databases/"+itoa(d.ID)+"/start", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start status = %d, want 303", resp.StatusCode)
	}
	if resp := e.postForm(t, "/databases/"+itoa(d.ID)+"/stop", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop status = %d, want 303", resp.StatusCode)
	}
	resp := e.postForm(t, "/databases/"+itoa(d.ID)+"/delete", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/projects/1" {
		t.Errorf("delete redirect = %q, want the project page", loc)
	}
	if len(e.databases.started) != 1 || len(e.databases.stopped) != 1 || len(e.databases.removed) != 1 {
		t.Errorf("manager calls = start %v stop %v remove %v, want one each",
			e.databases.started, e.databases.stopped, e.databases.removed)
	}
}

func TestDatabaseSettingsReprovisions(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)

	resp := e.postForm(t, "/databases/"+itoa(d.ID)+"/settings", url.Values{"external_port": {"5433"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, err := e.store.GetDatabase(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtPort != 5433 {
		t.Errorf("stored port = %d, want 5433", got.ExtPort)
	}
	if len(e.databases.provisioned) != 1 || e.databases.provisioned[0].ExtPort != 5433 {
		t.Fatalf("provisioned = %+v, want a reprovision with the new port", e.databases.provisioned)
	}
}

func TestDatabaseUnknown404(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	if resp := e.get(t, "/databases/9999"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestProjectPageListsDatabases(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)

	page := body(t, e.get(t, "/projects/1"))
	if !strings.Contains(page, "/databases/"+itoa(d.ID)) {
		t.Error("project page missing the database row link")
	}
	if !strings.Contains(page, "/projects/1/databases") {
		t.Error("project page missing the create-database form")
	}
	// The delete guard must acknowledge databases too.
	if !strings.Contains(page, "1 database") {
		t.Error("danger zone does not count the database")
	}
}

func TestDatabasesListAcrossProjects(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	seedDatabase(t, e, "shop-db", core.EnginePostgres) // default project
	p, err := e.store.CreateProject(context.Background(), core.Project{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateDatabase(context.Background(), core.Database{
		ProjectID: p.ID, Name: "cache", Engine: core.EngineRedis, Image: "redis:7", Password: "pw",
	}); err != nil {
		t.Fatal(err)
	}

	resp := e.get(t, "/databases")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /databases = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	for _, want := range []string{"shop-db", "cache", "acme", "/projects/" + itoa(p.ID)} {
		if !strings.Contains(page, want) {
			t.Errorf("databases page missing %q", want)
		}
	}
	if strings.Contains(page, "coming soon") {
		t.Error("databases page still renders as a placeholder")
	}
}

func TestDatabaseLogsSSEStreams(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	e.runtime.container = &docker.Container{ID: "db1", Name: "outhaul-db-shop", State: "running"}
	e.runtime.logs = map[string]string{"db1": "ready to accept connections\n"}

	got := body(t, e.get(t, "/databases/"+itoa(d.ID)+"/logs"))
	if !strings.Contains(got, "event: log") || !strings.Contains(got, "ready to accept connections") {
		t.Errorf("SSE body = %q, want the container's log line", got)
	}
}
