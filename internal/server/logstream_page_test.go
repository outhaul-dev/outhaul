package server

import (
	"context"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestLogStreamScriptIsServed(t *testing.T) {
	e := newTestEnv(t)
	page := body(t, e.get(t, "/static/logstream.js"))
	if !strings.Contains(page, "outhaulLogStream") {
		t.Error("logstream.js does not define outhaulLogStream")
	}
}

func TestLogPagesLoadTheSharedStreamScript(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")

	check := func(name, path string) {
		t.Helper()
		page := body(t, e.get(t, path))
		if !strings.Contains(page, "/static/logstream.js") {
			t.Errorf("%s page does not load logstream.js", name)
		}
		if !strings.Contains(page, "outhaulLogStream(") {
			t.Errorf("%s page does not use outhaulLogStream", name)
		}
		if strings.Contains(page, "new EventSource(") {
			t.Errorf("%s page still opens its own EventSource", name)
		}
	}
	check("app", "/apps/"+itoa(app.ID))
	d := seedDatabase(t, e, "shop", core.EnginePostgres)
	check("database", "/databases/"+itoa(d.ID))
	dep, err := e.store.CreateDeployment(context.Background(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("deployment", "/deployments/"+itoa(dep.ID))
}
