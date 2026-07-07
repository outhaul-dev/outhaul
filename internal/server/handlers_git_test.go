package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/github"
)

// testRSAKeyPEM generates a throwaway RSA private key PEM for constructing a
// core.GithubApp record in tests (github.AppJWT requires a parseable key).
func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
}

func TestCreateAppSSHGeneratesKey(t *testing.T) {
	env := newTestEnv(t)
	env.login(t) // helper: performs setup+login so requireAuth passes (see harness)

	form := url.Values{
		"name": {"svc"}, "domain": {"svc.example.com"}, "source": {"ssh"},
		"repo_url": {"git@github.com:o/r.git"}, "branch": {"main"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req) // helper: attach session cookie
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body)
	}

	app, err := env.store.GetAppByName(context.Background(), "svc")
	if err != nil {
		t.Fatal(err)
	}
	if app.Source != core.SourceSSH {
		t.Errorf("source = %q", app.Source)
	}
	if !strings.HasPrefix(app.SSHPublicKey, "ssh-ed25519 ") {
		t.Errorf("public key = %q", app.SSHPublicKey)
	}
	if app.WebhookSecret == "" {
		t.Error("webhook secret not generated")
	}
	key, _ := env.store.SSHPrivateKey(context.Background(), app.ID)
	if key == "" {
		t.Error("private key not stored")
	}
}

func TestCreateAppSSHRejectsDashRepo(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	form := url.Values{
		"name": {"evil"}, "domain": {"evil.example.com"}, "source": {"ssh"},
		"repo_url": {"-upload-pack=evil"}, "branch": {"main"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if _, err := env.store.GetAppByName(context.Background(), "evil"); err == nil {
		t.Error("app with dash-prefixed ssh repo_url should have been rejected")
	}
}

func TestCreateAppGithubUsesRepo(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	form := url.Values{
		"name": {"prod"}, "domain": {"prod.example.com"}, "source": {"github"},
		"github_repo": {"o/r"}, "branch": {"main"}, "auto_deploy": {"on"},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	app, _ := env.store.GetAppByName(context.Background(), "prod")
	if app.Source != core.SourceGithub || app.GithubRepo != "o/r" || !app.AutoDeploy {
		t.Errorf("app = %+v", app)
	}
	if app.RepoURL != "https://github.com/o/r.git" {
		t.Errorf("repo_url = %q", app.RepoURL)
	}
}

func TestUpdateAppSettingsHandler(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", WebhookSecret: "w",
	})
	form := url.Values{"branch": {"release"}, "auto_deploy": {"on"}}
	req := httptest.NewRequest("POST", "/apps/"+itoa(app.ID)+"/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	got, _ := env.store.GetApp(context.Background(), app.ID)
	if got.Branch != "release" || !got.AutoDeploy {
		t.Errorf("settings not applied: %+v", got)
	}
}

func TestAppsListShowsGithubReposWhenConnected(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	if err := env.store.SetGithubApp(ctx, core.GithubApp{
		AppID: 1, Slug: "s", PrivateKey: testRSAKeyPEM(t), WebhookSecret: "w", ClientID: "c", ClientSecret: "cs",
	}); err != nil {
		t.Fatalf("SetGithubApp: %v", err)
	}
	if err := env.store.SetInstallationID(ctx, 42); err != nil {
		t.Fatalf("SetInstallationID: %v", err)
	}
	env.gh.Token = "tok"
	env.gh.Repos = []github.Repo{{FullName: "o/r"}}

	resp := env.get(t, "/apps")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "o/r") {
		t.Error("expected connected GitHub repo o/r to be listed on the apps page")
	}
}

// TestAppsListDegradesGracefullyOnRepoListError verifies a GitHub API failure
// while listing repos does not break the apps page — it should just render
// without a repo dropdown.
func TestAppsListDegradesGracefullyOnRepoListError(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	if err := env.store.SetGithubApp(ctx, core.GithubApp{
		AppID: 1, Slug: "s", PrivateKey: testRSAKeyPEM(t), WebhookSecret: "w", ClientID: "c", ClientSecret: "cs",
	}); err != nil {
		t.Fatalf("SetGithubApp: %v", err)
	}
	if err := env.store.SetInstallationID(ctx, 42); err != nil {
		t.Fatalf("SetInstallationID: %v", err)
	}
	env.gh.ReposErr = context.DeadlineExceeded

	resp := env.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when repo listing fails", resp.StatusCode)
	}
}

func TestUpdateAppKindHandler(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	form := url.Values{"kind": {"dockerfile"}, "dockerfile_path": {"build/Dockerfile"}}
	resp := env.postForm(t, "/apps/"+itoa(app.ID)+"/kind", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, _ := env.store.GetApp(context.Background(), app.ID)
	if got.Kind != core.KindDockerfile || got.DockerfilePath != "build/Dockerfile" {
		t.Fatalf("kind not applied: %+v", got)
	}

	// invalid kind is rejected
	bad := env.postForm(t, "/apps/"+itoa(app.ID)+"/kind", url.Values{"kind": {"bogus"}})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid kind status = %d, want 400", bad.StatusCode)
	}
}
