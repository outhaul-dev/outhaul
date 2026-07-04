package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
)

// watchedApp creates an auto-deploying app watching the given paths.
func watchedApp(t *testing.T, env *testEnv, watch []string) core.App {
	t.Helper()
	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", AutoDeploy: true, WebhookSecret: "tok",
		WatchPaths: watch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func pushWebhook(t *testing.T, env *testEnv, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("tok", body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func deployCount(t *testing.T, env *testEnv, appID int64) int {
	t.Helper()
	deps, err := env.store.ListDeploymentsForApp(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return len(deps)
}

func TestWebhookWatchPathsMatchDeploys(t *testing.T) {
	env := newTestEnv(t)
	app := watchedApp(t, env, []string{"src/**"})

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"},
		"commits":[{"modified":["src/lib/app.js"]}]}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("deployments = %d, want 1 (src change matches src/**)", n)
	}
}

func TestWebhookWatchPathsMissSkips(t *testing.T) {
	env := newTestEnv(t)
	app := watchedApp(t, env, []string{"src/**"})

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"},
		"commits":[{"modified":["docs/README.md"],"added":["docs/new.md"]}]}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op", code)
	}
	if n := deployCount(t, env, app.ID); n != 0 {
		t.Errorf("deployments = %d, want 0 (docs-only push must not deploy)", n)
	}
}

// A payload with no file info at all (branch creation, thin webhook) fails
// open: not knowing what changed must never silently drop a release.
func TestWebhookWatchPathsFailOpenWithoutFileInfo(t *testing.T) {
	env := newTestEnv(t)
	app := watchedApp(t, env, []string{"src/**"})

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"},"commits":[{"id":"abc"}]}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("deployments = %d, want 1 (fail open on missing file info)", n)
	}
}

func TestWebhookNoWatchPathsDeploysOnAnyChange(t *testing.T) {
	env := newTestEnv(t)
	app := watchedApp(t, env, nil)

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"},
		"commits":[{"modified":["docs/README.md"]}]}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("deployments = %d, want 1 (no watch paths = every push)", n)
	}
}
