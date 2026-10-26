package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/store"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// tagApp creates an auto-deploying public-source app in tag mode.
func tagApp(t *testing.T, env *testEnv, pattern string, pre bool) core.App {
	t.Helper()
	ctx := context.Background()
	app, err := env.store.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", AutoDeploy: true, WebhookToken: "tok", WebhookSecret: "sek",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, app.ID, pattern, pre, true)
	app, err = env.store.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func setTagMode(t *testing.T, env *testEnv, id int64, pattern string, pre, auto bool) {
	t.Helper()
	err := env.store.UpdateAppSettings(context.Background(), id, store.DeploySettings{
		Branch: "main", AutoDeploy: auto, Trigger: core.TriggerTag, TagPattern: pattern, TagPrereleases: pre,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pushTagBody(tag string, deleted bool) string {
	after := "1111111111111111111111111111111111111111"
	del := "false"
	if deleted {
		after, del = zeroSHA, "true"
	}
	return `{"ref":"refs/tags/` + tag + `","after":"` + after + `","deleted":` + del + `,"repository":{"full_name":"o/r"}}`
}

func pushWebhookRec(t *testing.T, env *testEnv, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("sek", body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func markRunning(t *testing.T, env *testEnv, id int64) {
	t.Helper()
	from := core.StatusQueued
	for _, to := range []core.DeployStatus{core.StatusBuilding, core.StatusDeploying, core.StatusRunning} {
		if ok, err := env.store.SetStatus(context.Background(), id, from, to, ""); err != nil || !ok {
			t.Fatalf("%s -> %s: ok=%v err=%v", from, to, ok, err)
		}
		from = to
	}
}

func seedLiveTag(t *testing.T, env *testEnv, appID int64, tag string) {
	t.Helper()
	ctx := context.Background()
	dep, ok, err := env.store.EnqueueTagDeployment(ctx, appID, func([]string) (string, bool) { return tag, true })
	if err != nil || !ok {
		t.Fatalf("seed tag %s: ok=%v err=%v", tag, ok, err)
	}
	markRunning(t, env, dep.ID)
}

func TestTagAppDeploysNewTag(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)

	rec := pushWebhookRec(t, env, pushTagBody("v1.4.0", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	deps, _ := env.store.ListDeploymentsForApp(context.Background(), app.ID)
	if len(deps) != 1 || deps[0].Tag != "v1.4.0" || !deps[0].FromTagEvent {
		t.Fatalf("deployments = %+v, want one for v1.4.0 from a tag event", deps)
	}
	if !strings.Contains(rec.Body.String(), "web: deploying v1.4.0") {
		t.Errorf("body = %q", rec.Body)
	}
}

func TestTagAppIgnoresBranchPush(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"}}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 0 {
		t.Errorf("deployments = %d, want 0", n)
	}
}

func TestTagAppSkipsOlderTagAndSaysWhy(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	seedLiveTag(t, env, app.ID, "v1.4.0")

	rec := pushWebhookRec(t, env, pushTagBody("v1.3.1", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("deployments = %d, want 1 (the seeded one)", n)
	}
	if !strings.Contains(rec.Body.String(), "v1.3.1 is not newer than v1.4.0") {
		t.Errorf("body = %q, want the reason", rec.Body)
	}
}

func TestTagAppIgnoresDeletedTag(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	if code := pushWebhook(t, env, pushTagBody("v1.4.0", true)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 0 {
		t.Errorf("deployments = %d, want 0", n)
	}
}

func TestTagAppPreReleases(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	if code := pushWebhook(t, env, pushTagBody("v1.5.0-rc.1", false)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 0 {
		t.Errorf("pre-releases off: deployments = %d, want 0", n)
	}
	setTagMode(t, env, app.ID, "v*", true, true)
	if code := pushWebhook(t, env, pushTagBody("v1.5.0-rc.1", false)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("pre-releases on: deployments = %d, want 1", n)
	}
}

func TestTagAppAutoDeployOffDoesNothing(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	setTagMode(t, env, app.ID, "v*", false, false)
	rec := pushWebhookRec(t, env, pushTagBody("v1.4.0", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := deployCount(t, env, app.ID); n != 0 {
		t.Errorf("deployments = %d, want 0", n)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body)
	}
}

func TestTagAppWatchPathsDontApply(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	err := env.store.UpdateAppSettings(context.Background(), app.ID, store.DeploySettings{
		Branch: "main", AutoDeploy: true, WatchPaths: []string{"src/**"},
		Trigger: core.TriggerTag, TagPattern: "v*",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"ref":"refs/tags/v1.0.0","repository":{"full_name":"o/r"},"commits":[{"modified":["docs/x.md"]}]}`
	if code := pushWebhook(t, env, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := deployCount(t, env, app.ID); n != 1 {
		t.Errorf("deployments = %d, want 1 (watch paths do not apply to tags)", n)
	}
}

func TestTagAppFirstTagAfterBranchDeploy(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	ctx := context.Background()
	dep, err := env.store.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	markRunning(t, env, dep.ID)
	if code := pushWebhook(t, env, pushTagBody("v0.1.0", false)); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	deps, _ := env.store.ListDeploymentsForApp(ctx, app.ID)
	var tagged int
	for _, d := range deps {
		if d.Tag == "v0.1.0" {
			tagged++
		}
	}
	if len(deps) != 2 || tagged != 1 {
		t.Errorf("deployments = %+v, want a new one for v0.1.0", deps)
	}
}

// postGithubPayload delivers a signed GitHub App push payload.
func postGithubPayload(t *testing.T, env *testEnv, appID int64, secret, payload string) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(appID, 10))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestGithubAppWebhookStagingAndProduction(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	srcID := connectApp(t, env, 1, "s")
	stage, _ := env.store.CreateApp(ctx, core.App{
		Name: "stage", RepoURL: "x", Domain: "stage.example.com",
		Source: core.SourceGithub, GithubRepo: "o/r", GitSourceID: srcID, Branch: "main", AutoDeploy: true, WebhookSecret: "t1",
	})
	prod, _ := env.store.CreateApp(ctx, core.App{
		Name: "prod", RepoURL: "x", Domain: "prod.example.com",
		Source: core.SourceGithub, GithubRepo: "o/r", GitSourceID: srcID, Branch: "main", AutoDeploy: true, WebhookSecret: "t2",
	})
	setTagMode(t, env, prod.ID, "v*", false, true)

	rec := postGithubPayload(t, env, 1, "whs-s", pushTagBody("v2.0.0", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("tag push status = %d; body=%s", rec.Code, rec.Body)
	}
	if n := deployCount(t, env, prod.ID); n != 1 {
		t.Errorf("prod deployments = %d, want 1", n)
	}
	if n := deployCount(t, env, stage.ID); n != 0 {
		t.Errorf("stage deployments = %d, want 0", n)
	}

	rec = postPush(t, env, 1, "whs-s", "o/r", "main")
	if rec.Code != http.StatusOK {
		t.Fatalf("branch push status = %d; body=%s", rec.Code, rec.Body)
	}
	if n := deployCount(t, env, stage.ID); n != 1 {
		t.Errorf("stage deployments = %d, want 1", n)
	}
	if n := deployCount(t, env, prod.ID); n != 1 {
		t.Errorf("prod deployments = %d, want still 1", n)
	}
}

func TestGitlabTagPushHookDeploys(t *testing.T) {
	env := newTestEnv(t)
	app := tagApp(t, env, "v*", false)
	body := `{"object_kind":"tag_push","ref":"refs/tags/v1.0.0","before":"` + zeroSHA +
		`","after":"1111111111111111111111111111111111111111","project":{"path_with_namespace":"g/p"}}`
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "sek")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body)
	}
	deps, _ := env.store.ListDeploymentsForApp(context.Background(), app.ID)
	if len(deps) != 1 || deps[0].Tag != "v1.0.0" {
		t.Errorf("deployments = %+v, want one for v1.0.0", deps)
	}
}
