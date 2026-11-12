package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
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
	// The delete guard must acknowledge databases too. The default project
	// can't be deleted at all, so this needs another one.
	p, err := e.store.CreateProject(context.Background(), core.Project{Name: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateDatabase(context.Background(), core.Database{
		ProjectID: p.ID, Name: "shop-db", Engine: core.EnginePostgres, Image: "postgres:17", Username: "shop", DBName: "shop", Password: "pw123",
	}); err != nil {
		t.Fatal(err)
	}
	if page := body(t, e.get(t, "/projects/"+itoa(p.ID))); !strings.Contains(page, "1 database") {
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

// Under the strict container policy the database forms refuse a host port
// outright: nothing is stored and nothing is provisioned.
func TestStrictPolicyRefusesAPortFromTheForms(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	e.srv.SetContainerPolicy(true)

	resp := e.postForm(t, "/projects/1/databases", url.Values{
		"name": {"shop"}, "engine": {"postgres"}, "external_port": {"5433"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "container policy") || !strings.Contains(got, "published port") {
		t.Errorf("create refusal does not name the container policy and published ports")
	}
	if len(e.databases.provisioned) != 0 {
		t.Errorf("refused create provisioned %+v", e.databases.provisioned)
	}
	if dbs, err := e.store.ListDatabases(context.Background()); err != nil || len(dbs) != 0 {
		t.Errorf("refused create stored %+v (err %v), want no row", dbs, err)
	}

	d := seedDatabase(t, e, "stock", core.EnginePostgres)
	if err := e.store.SetDatabaseExtPort(context.Background(), d.ID, 5434); err != nil {
		t.Fatal(err)
	}
	resp = e.postForm(t, "/databases/"+itoa(d.ID)+"/settings", url.Values{"external_port": {"5433"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("settings status = %d, want 400", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "container policy") || !strings.Contains(got, "published port") {
		t.Errorf("settings refusal does not name the container policy and published ports")
	}
	after, err := e.store.GetDatabase(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExtPort != 5434 {
		t.Errorf("refused settings stored port = %d, want the existing 5434 kept", after.ExtPort)
	}
	if len(e.databases.provisioned) != 0 {
		t.Errorf("refused settings provisioned %+v", e.databases.provisioned)
	}
}

// The strict policy takes the external-port field off both database forms; it
// is the setting, not the page, that decides.
func TestStrictPolicyHidesThePortField(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	dbPath := "/databases/" + itoa(d.ID)

	for _, path := range []string{dbPath, "/projects/1"} {
		if page := body(t, e.get(t, path)); !strings.Contains(page, `name="external_port"`) {
			t.Fatalf("%s has no external-port field before the strict policy", path)
		}
	}

	e.srv.SetContainerPolicy(true)
	for _, path := range []string{dbPath, "/projects/1"} {
		if page := body(t, e.get(t, path)); strings.Contains(page, `name="external_port"`) {
			t.Errorf("%s still offers an external-port field under the strict policy", path)
		}
	}
}

// A row that still publishes a port under the strict policy gets a notice and
// one button that posts an empty port to the settings route, which clears it.
func TestStrictPolicyOffersToStopAnExistingPort(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	if err := e.store.SetDatabaseExtPort(context.Background(), d.ID, 5433); err != nil {
		t.Fatal(err)
	}
	e.srv.SetContainerPolicy(true)
	dbPath := "/databases/" + itoa(d.ID)

	page := body(t, e.get(t, dbPath))
	if !strings.Contains(page, "Stop publishing port 5433") {
		t.Error("page does not offer to stop publishing the port")
	}
	if strings.Contains(page, `<input type="text" name="external_port"`) {
		t.Error("page still offers a text field for the port")
	}
	form := regexp.MustCompile(`(?s)<form[^>]*action="` + dbPath + `/settings"[^>]*>.*?</form>`)
	offered := false
	for _, f := range form.FindAllString(page, -1) {
		offered = offered || strings.Contains(f, `name="external_port"`) && strings.Contains(f, `value=""`)
	}
	if !offered {
		t.Error("no form posts an empty external_port to the settings route")
	}

	resp := e.postForm(t, dbPath+"/settings", url.Values{"external_port": {""}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop-publishing status = %d, want 303", resp.StatusCode)
	}
	got, err := e.store.GetDatabase(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtPort != 0 {
		t.Errorf("stored port = %d, want 0", got.ExtPort)
	}
	if len(e.databases.provisioned) != 1 || e.databases.provisioned[0].ExtPort != 0 {
		t.Errorf("provisioned = %+v, want a reprovision with no port", e.databases.provisioned)
	}
}

// A read-only viewer sees the warning about a database that still publishes a
// port under the strict policy, but gets no form to change it.
func TestViewerSeesTheStrictPortWarningWithoutTheForm(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	if err := e.store.SetDatabaseExtPort(context.Background(), d.ID, 5433); err != nil {
		t.Fatal(err)
	}
	e.srv.SetContainerPolicy(true)
	viewer := e.signInAs(t, "vera", core.RoleProjectViewer, d.ProjectID)

	page := body(t, e.getAs(t, viewer, "/databases/"+itoa(d.ID)))
	if !strings.Contains(page, "still publishes port 5433") {
		t.Error("viewer page does not warn that the database still publishes its port")
	}
	for _, unwanted := range []string{"Stop publishing port", `name="external_port"`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("viewer page offers %q, want the warning only", unwanted)
		}
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
