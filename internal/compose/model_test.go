package compose

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"
)

// readFixture loads one of testdata/model's recorded compose configs.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/model/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// normalised.json is the recorded output of
//
//	docker compose -p demo -f compose.yaml config --format json --no-env-resolution
//
// for a stack whose web service only builds and whose db service sets
// restart: always and deploy.restart_policy: on-failure. The source set
// mem_limit: 0, which Compose leaves out of its output; the recorded build
// context was made relative again so the fixture is machine-independent.
// unknown-keys.json is hand-written: Compose drops service-level x-* keys, so
// a service that keeps one cannot be recorded.
func TestParseModel(t *testing.T) {
	m, err := ParseModel(readFixture(t, "normalised.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Project != "demo" {
		t.Errorf("Project = %q, want demo", m.Project)
	}
	if len(m.Services) != 2 || m.Services[0].Name != "db" || m.Services[1].Name != "web" {
		t.Fatalf("Services = %+v, want db then web", m.Services)
	}

	db := m.Services[0]
	if db.Image != "postgres:16" {
		t.Errorf("db.Image = %q, want postgres:16", db.Image)
	}
	if db.Restart != "always" {
		t.Errorf("db.Restart = %q, want always", db.Restart)
	}
	if !db.DeployRestartPolicy {
		t.Error("db.DeployRestartPolicy = false, want true")
	}
	if !db.HasRestart() {
		t.Error("db.HasRestart() = false, want true")
	}
	if db.Labels["com.example.site"] != "db.example.com" {
		t.Errorf("db.Labels = %v", db.Labels)
	}
	if string(db.Raw["image"]) != `"postgres:16"` {
		t.Errorf("db.Raw[image] = %s", db.Raw["image"])
	}

	web := m.Services[1]
	if web.Image != "demo-web" {
		t.Errorf("web.Image = %q, want demo-web (project + service for a build-only service)", web.Image)
	}
	if web.Restart != "" || web.DeployRestartPolicy || web.HasRestart() {
		t.Errorf("web has a restart policy: %+v", web)
	}
	if _, ok := web.Raw["build"]; !ok {
		t.Error("web.Raw lost its build key")
	}
	if web.Raw["image"] != nil {
		t.Errorf("web.Raw[image] = %s, want no image key", web.Raw["image"])
	}

	// unknown-keys.json lists web before api and keeps a service-level x-* key.
	m, err = ParseModel(readFixture(t, "unknown-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Services) != 2 || m.Services[0].Name != "api" || m.Services[1].Name != "web" {
		t.Fatalf("Services = %+v, want api then web", m.Services)
	}
	if got := string(m.Services[1].Raw["x-example"]); got != `"kept"` {
		t.Errorf("web.Raw[x-example] = %s, want \"kept\"", got)
	}
	if m.Services[0].Image != "unknown-api" {
		t.Errorf("api.Image = %q, want unknown-api", m.Services[0].Image)
	}
}

func TestParseModelKeepsUnknownTopLevelKeys(t *testing.T) {
	m, err := ParseModel(readFixture(t, "unknown-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(m.Raw["x-example"]); got != `"kept"` {
		t.Errorf("Raw[x-example] = %s, want \"kept\"", got)
	}
	if string(m.Raw["services"]) == "" {
		t.Error("Raw lost the services section")
	}
}

func TestReadModelUsesTheRunner(t *testing.T) {
	f := &Fake{ConfigJSON: readFixture(t, "normalised.json")}
	files := []string{"docker-compose.yml", OverrideFile}
	m, err := ReadModel(context.Background(), f, "work", files, "outhaul-demo", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if m.Project != "demo" || len(m.Services) != 2 {
		t.Errorf("model = %+v, want the parsed normalised config", m)
	}
	calls := f.CallsFor("config")
	if len(calls) != 1 {
		t.Fatalf("config calls = %d, want 1", len(calls))
	}
	want := Call{Verb: "config", Dir: "work", Files: files, Project: "outhaul-demo"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Errorf("config call = %+v, want %+v", calls[0], want)
	}
}
