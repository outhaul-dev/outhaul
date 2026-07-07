package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/webhook"
)

// fakePreviews records the last pull_request event routed to it.
type fakePreviews struct {
	last  webhook.PullRequestEvent
	calls int
}

func (f *fakePreviews) Handle(_ context.Context, ev webhook.PullRequestEvent) error {
	f.calls++
	f.last = ev
	return nil
}

func TestGithubWebhookRoutesPullRequest(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.store.SetGithubApp(ctx, core.GithubApp{
		AppID: 1, Slug: "s", PrivateKey: "p", WebhookSecret: "ghwhs", ClientID: "c", ClientSecret: "cs",
	})
	fake := &fakePreviews{}
	env.srv.previews = fake

	body := `{"action":"opened","number":42,"pull_request":{"head":{"ref":"feature-x","sha":"abc123","repo":{"full_name":"me/app"}},"base":{"repo":{"full_name":"me/app"}}}}`
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("ghwhs", body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
	if fake.last.Action != "opened" || fake.last.Number != 42 || fake.last.BaseRepoFullName != "me/app" {
		t.Errorf("last event = %+v, want opened/42/me/app", fake.last)
	}
}

func TestGithubWebhookPullRequestNilPreviewsIsNoop(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.store.SetGithubApp(ctx, core.GithubApp{
		AppID: 1, Slug: "s", PrivateKey: "p", WebhookSecret: "ghwhs", ClientID: "c", ClientSecret: "cs",
	})
	// env.srv.previews is nil (harness default).

	body := `{"action":"opened","number":42,"pull_request":{"head":{"ref":"feature-x","sha":"abc123","repo":{"full_name":"me/app"}},"base":{"repo":{"full_name":"me/app"}}}}`
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("ghwhs", body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op", rec.Code)
	}
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestAppWebhookEnqueuesOnMatch(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	app, _ := env.store.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", AutoDeploy: true, WebhookSecret: "tok",
	})

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"}}`
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("tok", body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	deps, _ := env.store.ListDeploymentsForApp(ctx, app.ID)
	if len(deps) != 1 {
		t.Errorf("deployments = %d, want 1", len(deps))
	}
	if env.deployer.notified == 0 {
		t.Error("deployer was not notified")
	}
}

func TestAppWebhookNoDeployOnBranchMismatch(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	app, _ := env.store.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", AutoDeploy: true, WebhookSecret: "tok",
	})
	body := `{"ref":"refs/heads/other","repository":{"full_name":"o/r"}}`
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("tok", body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op", rec.Code)
	}
	deps, _ := env.store.ListDeploymentsForApp(ctx, app.ID)
	if len(deps) != 0 {
		t.Errorf("deployments = %d, want 0 (branch mismatch)", len(deps))
	}
}

func TestAppWebhookBadSignature(t *testing.T) {
	env := newTestEnv(t)
	env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", AutoDeploy: true, WebhookSecret: "tok",
	})
	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"}}`
	req := httptest.NewRequest("POST", "/webhooks/app/tok", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("WRONG", body))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAppWebhookUnknownToken(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest("POST", "/webhooks/app/nope", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGithubAppWebhookFansOut(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	// Configure the App (sets the webhook secret used to verify).
	env.store.SetGithubApp(ctx, core.GithubApp{
		AppID: 1, Slug: "s", PrivateKey: "p", WebhookSecret: "ghwhs", ClientID: "c", ClientSecret: "cs",
	})
	a1, _ := env.store.CreateApp(ctx, core.App{
		Name: "prod", RepoURL: "x", Domain: "prod.example.com",
		Source: core.SourceGithub, GithubRepo: "o/r", Branch: "main", AutoDeploy: true, WebhookSecret: "t1",
	})
	// Same repo, non-matching branch -> should not deploy.
	env.store.CreateApp(ctx, core.App{
		Name: "stage", RepoURL: "x", Domain: "stage.example.com",
		Source: core.SourceGithub, GithubRepo: "o/r", Branch: "develop", AutoDeploy: true, WebhookSecret: "t2",
	})

	body := `{"ref":"refs/heads/main","repository":{"full_name":"o/r"}}`
	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign("ghwhs", body))
	req.Header.Set("X-GitHub-Event", "push")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if d1, _ := env.store.ListDeploymentsForApp(ctx, a1.ID); len(d1) != 1 {
		t.Errorf("prod deployments = %d, want 1", len(d1))
	}
}
