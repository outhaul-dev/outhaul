package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

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
	id := installSource(t, env, 55, "outhaul-a", 9001, "o", "User")
	env.gh.Repos = []github.Repo{{FullName: "o/r", DefaultBranch: "main"}}
	form := url.Values{
		"name": {"prod"}, "domain": {"prod.example.com"}, "source": {"github"},
		"github_repo": {"o/r"}, "git_source_id": {strconv.FormatInt(id, 10)},
		"branch": {"main"}, "auto_deploy": {"on"},
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
	installSource(t, env, 55, "outhaul-a", 9001, "o", "User")
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

// TestGithubReposAreCachedAcrossRenders verifies the repo list is fetched once
// and reused on subsequent page renders, rather than paying two api.github.com
// round-trips every time an app/create/project page is opened.
func TestGithubReposAreCachedAcrossRenders(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "o", "User")
	env.gh.Token = "tok"
	env.gh.Repos = []github.Repo{{FullName: "o/r"}}

	for i := 0; i < 3; i++ {
		resp := env.get(t, "/apps")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("render %d: status = %d, want 200", i, resp.StatusCode)
		}
		body(t, resp)
	}
	if env.gh.ReposCalls != 1 {
		t.Errorf("ListRepos called %d times across 3 renders, want 1 (cached)", env.gh.ReposCalls)
	}
}

// TestGithubReposFailureIsCachedAcrossRenders verifies a source whose repo
// fetch fails is only probed once per ghRepoTTL window, not once per render —
// otherwise a permanently-unreachable source (App deleted, key revoked, no
// network) pays two api.github.com round-trips' worth of latency on every
// page load, forever.
func TestGithubReposFailureIsCachedAcrossRenders(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "o", "User")
	env.gh.ReposErr = context.DeadlineExceeded

	for i := 0; i < 2; i++ {
		resp := env.get(t, "/apps")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("render %d: status = %d, want 200", i, resp.StatusCode)
		}
		body(t, resp)
	}
	if env.gh.ReposCalls != 1 {
		t.Errorf("ListRepos called %d times across 2 renders of a failing source, want 1 (failure cached)", env.gh.ReposCalls)
	}
}

// TestAppsListDegradesGracefullyOnRepoListError verifies a GitHub API failure
// while listing repos does not break the apps page — it should just render
// without a repo dropdown.
func TestAppsListDegradesGracefullyOnRepoListError(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "o", "User")
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

func TestUpdateAppSourceHandler(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})

	// public -> ssh regenerates a deploy key
	resp := env.postForm(t, "/apps/"+itoa(app.ID)+"/source", url.Values{
		"source": {"ssh"}, "repo_url": {"git@github.com:o/r.git"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ssh switch status = %d, want 303", resp.StatusCode)
	}
	got, _ := env.store.GetApp(context.Background(), app.ID)
	if got.Source != core.SourceSSH || got.SSHPublicKey == "" {
		t.Fatalf("ssh source not applied: %+v", got)
	}

	// switch to push clears the repo URL
	resp = env.postForm(t, "/apps/"+itoa(app.ID)+"/source", url.Values{"source": {"push"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("push switch status = %d, want 303", resp.StatusCode)
	}
	got, _ = env.store.GetApp(context.Background(), app.ID)
	if got.Source != core.SourcePush || got.RepoURL != "" || got.SSHPublicKey != "" {
		t.Fatalf("push source not applied: %+v", got)
	}

	// invalid public URL is rejected
	bad := env.postForm(t, "/apps/"+itoa(app.ID)+"/source", url.Values{
		"source": {"public"}, "repo_url": {"not-a-url"},
	})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad public url status = %d, want 400", bad.StatusCode)
	}
}

// TestUpdateAppSourceClearsStaleGithubRepo covers switching a github-sourced
// app away to public/ssh/push: git_source_id is reset to 0 in that branch, and
// github_repo must be cleared alongside it rather than keeping whatever the
// form submitted — a stale repo name next to git_source_id = 0 is a value
// nothing validates any more, and AppsUsingGitSource's delete guard assumes
// the two only ever mean something together.
func TestUpdateAppSourceClearsStaleGithubRepo(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.gh.Repos = []github.Repo{{FullName: "jsmart/outhaul", DefaultBranch: "main"}}

	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "web3", RepoURL: "https://github.com/jsmart/outhaul.git", Domain: "web3.test",
		Source: core.SourceGithub, GithubRepo: "jsmart/outhaul", GitSourceID: id,
		Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	resp := env.postForm(t, "/apps/"+itoa(app.ID)+"/source", url.Values{
		"source": {"public"}, "repo_url": {"https://example.com/other.git"},
		// A stale github_repo value, as a leftover hidden field would carry.
		"github_repo": {"jsmart/outhaul"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, err := env.store.GetApp(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.GithubRepo != "" {
		t.Errorf("GithubRepo = %q, want cleared alongside GitSourceID", got.GithubRepo)
	}
	if got.GitSourceID != 0 {
		t.Errorf("GitSourceID = %d, want 0", got.GitSourceID)
	}
}
