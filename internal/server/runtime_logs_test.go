package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/docker"
)

// seedRunningApp creates a nixpacks app with a running container whose logs
// the fake runtime will serve.
func seedRunningApp(t *testing.T, e *testEnv, logs string) core.App {
	t.Helper()
	app, err := e.store.CreateApp(context.Background(),
		core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}
	e.runtime.container = &docker.Container{ID: "c1", Name: "outhaul-app-web", State: "running"}
	e.runtime.logs = map[string]string{"c1": logs}
	return app
}

func TestRuntimeLogsSSE(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "listening on :3000\nGET / 200\n")

	resp := e.get(t, "/apps/"+itoa(app.ID)+"/logs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	stream := body(t, resp)
	if !strings.Contains(stream, "event: log\ndata: listening on :3000\n") {
		t.Errorf("stream missing first log line:\n%s", stream)
	}
	if !strings.Contains(stream, "data: GET / 200\n") {
		t.Errorf("stream missing second log line:\n%s", stream)
	}
	if !strings.Contains(stream, "event: done") {
		t.Errorf("stream should end with a done event:\n%s", stream)
	}
	if got := e.runtime.logTails; len(got) != 1 || got[0] != 100 {
		t.Errorf("tail = %v, want the 100-line default", got)
	}
}

func TestRuntimeLogsTailWhitelist(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "x\n")

	body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs?tail=500"))
	body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs?tail=999")) // not whitelisted
	if got := e.runtime.logTails; len(got) != 2 || got[0] != 500 || got[1] != 100 {
		t.Errorf("tails = %v, want [500 100]", got)
	}
}

func TestRuntimeLogsNoContainer(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app, err := e.store.CreateApp(context.Background(),
		core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatal(err)
	}

	stream := body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs"))
	if !strings.Contains(stream, "event: err") || !strings.Contains(stream, "deploy it first") {
		t.Errorf("want an err event telling the user to deploy; got:\n%s", stream)
	}
	if strings.Contains(stream, "event: done") {
		t.Errorf("no done event when nothing was streamed:\n%s", stream)
	}
}

func TestRuntimeLogsUnknownApp404(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/apps/9999/logs")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// seedComposeStack creates a compose app whose fake stack has web and db
// services with distinct log content.
func seedComposeStack(t *testing.T, e *testEnv) core.App {
	t.Helper()
	e.postForm(t, "/apps", composeForm("shop")).Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	e.runtime.stack = []docker.Container{
		{ID: "c1", Name: "outhaul-shop-web-1", State: "running",
			Labels: map[string]string{"com.docker.compose.service": "web"}},
		{ID: "c2", Name: "outhaul-shop-db-1", State: "running",
			Labels: map[string]string{"com.docker.compose.service": "db"}},
	}
	e.runtime.logs = map[string]string{"c1": "web says hi\n", "c2": "db ready\n"}
	return app
}

func TestRuntimeLogsComposeSelectsService(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app := seedComposeStack(t, e)

	stream := body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs?service=web"))
	if !strings.Contains(stream, "web says hi") || strings.Contains(stream, "db ready") {
		t.Errorf("service=web should tail only the web container:\n%s", stream)
	}

	// No service param: the first service alphabetically (db).
	stream = body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs"))
	if !strings.Contains(stream, "db ready") || strings.Contains(stream, "web says hi") {
		t.Errorf("default service should be the alphabetical first (db):\n%s", stream)
	}
}

func TestRuntimeLogsComposeUnknownService(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app := seedComposeStack(t, e)

	stream := body(t, e.get(t, "/apps/"+itoa(app.ID)+"/logs?service=ghost"))
	if !strings.Contains(stream, "event: err") || !strings.Contains(stream, "ghost") {
		t.Errorf("want an err event naming the unknown service:\n%s", stream)
	}
}

func TestAppPageShowsRuntimeLogsPanel(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	app := seedRunningApp(t, e, "")

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "Runtime logs") {
		t.Error("app page missing the Runtime logs panel")
	}
	if !strings.Contains(page, `id="log-tail"`) {
		t.Error("app page missing the tail selector")
	}
	if strings.Contains(page, `id="log-service"`) {
		t.Error("nixpacks apps have one container; no service selector expected")
	}
}

func TestComposeAppPageShowsServiceSelector(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app := seedComposeStack(t, e)

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, `id="log-service"`) {
		t.Error("compose app page missing the service selector")
	}
	for _, svc := range []string{`value="web"`, `value="db"`} {
		if !strings.Contains(page, svc) {
			t.Errorf("service selector missing option %s", svc)
		}
	}
}
