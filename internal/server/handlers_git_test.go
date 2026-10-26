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

// A refused create re-renders the form. It must come back as submitted: a
// GitHub App choice that fell back to "Public Git URL", with the derived
// https URL already in the Git URL box, was resubmitted and saved an app that
// clones a private repository without credentials.
func TestCreateAppErrorKeepsTheForm(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "o", "Organization")
	env.gh.Repos = []github.Repo{{FullName: "o/tals", DefaultBranch: "main"}}
	p, err := env.store.CreateProject(context.Background(), core.Project{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.FormatInt(p.ID, 10)
	form := url.Values{
		"name": {"tals"}, "source": {"github"}, "github_repo": {"o/tals"},
		"git_source_id": {strconv.FormatInt(id, 10)}, "branch": {"release"},
		"kind": {"compose"}, "compose_path": {"deploy/compose.yml"}, "dockerfile_path": {"build/Dockerfile"},
		"domain": {"tals.example.com"}, "compose_service": {"web"}, "compose_port": {"notaport"},
		"auto_deploy": {"on"}, "project_id": {pid}, "return": {"/projects/" + pid},
	}
	req := httptest.NewRequest("POST", "/apps", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{
		`name="name" placeholder="web" value="tals"`,
		`<option value="github" selected>`,
		`<option value="o/tals" data-source-id="` + strconv.FormatInt(id, 10) + `" data-default-branch="main" selected>`,
		`id="git-source-id" value="` + strconv.FormatInt(id, 10) + `"`,
		`id="branch-input" placeholder="main" value="release"`,
		`<option value="compose" selected>`,
		`name="dockerfile_path" placeholder="Dockerfile" value="build/Dockerfile"`,
		`name="compose_path" placeholder="docker-compose.yml" value="deploy/compose.yml"`,
		`name="domain" id="domain-input" placeholder="web.example.com" value="tals.example.com"`,
		`name="compose_service" placeholder="web" value="web"`,
		`name="compose_port" placeholder="3000" inputmode="numeric" value="notaport"`,
		`name="auto_deploy" checked`,
		`<option value="` + pid + `" selected>acme</option>`,
		`name="return" value="/projects/` + pid + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("re-rendered form lacks %s", want)
		}
	}
	// The Git URL box is for public and SSH sources; it must not carry the
	// URL derived from the GitHub repository.
	if strings.Contains(page, `value="https://github.com/o/tals.git"`) {
		t.Error("Git URL box pre-filled with the GitHub repository's URL")
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

// Before hostnames were validated strictly, the create form stored the domain
// as typed (Traefik matches Host case-insensitively, so it routed fine). A
// source change re-validates the stored domain and must not reject such an
// app on a form that doesn't touch its domain.
func TestUpdateAppSourceAcceptsLegacyMixedCaseDomain(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, _ := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://example.com/r.git", Domain: "Shop.Example.com",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	resp := env.postForm(t, "/apps/"+itoa(app.ID)+"/source", url.Values{
		"source": {"public"}, "repo_url": {"https://example.com/other.git"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("source change status = %d, want 303", resp.StatusCode)
	}
}

func postSettings(t *testing.T, env *testEnv, id int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/apps/"+itoa(id)+"/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	env.authed(req)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func settingsApp(t *testing.T, env *testEnv) core.App {
	t.Helper()
	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Source: core.SourcePublic, Branch: "main", WebhookSecret: "w",
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestUpdateAppSettingsTagMode(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app := settingsApp(t, env)
	rec := postSettings(t, env, app.ID, url.Values{
		"deploy_trigger": {"tag"}, "tag_pattern": {"release-*"}, "tag_prereleases": {"on"},
		"branch": {"main"}, "auto_deploy": {"on"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := env.store.GetApp(context.Background(), app.ID)
	if got.DeployTrigger != core.TriggerTag || got.TagPattern != "release-*" || !got.TagPrereleases ||
		got.Branch != "main" || !got.AutoDeploy {
		t.Errorf("settings not stored: %+v", got)
	}
}

func TestUpdateAppSettingsRejectsBadTagPattern(t *testing.T) {
	for _, tt := range []struct{ trigger, pattern, want string }{
		{"tag", "", "A tag pattern is required"},
		{"tag", "v 1*", "can't contain spaces"},
		{"tag", "v[0-9", "unclosed ["},
		{"nightly", "v*", "unknown deploy trigger"},
	} {
		t.Run(tt.trigger+"/"+tt.pattern, func(t *testing.T) {
			env := newTestEnv(t)
			env.login(t)
			app := settingsApp(t, env)
			rec := postSettings(t, env, app.ID, url.Values{
				"deploy_trigger": {tt.trigger}, "tag_pattern": {tt.pattern},
				"branch": {"changed"}, "auto_deploy": {"on"},
			})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("status = %d body = %q, want 400 containing %q", rec.Code, rec.Body.String(), tt.want)
			}
			got, _ := env.store.GetApp(context.Background(), app.ID)
			if got.Branch != "main" || got.AutoDeploy || got.DeployTrigger != core.TriggerBranch {
				t.Errorf("app changed despite the error: %+v", got)
			}
		})
	}
}

func TestUpdateAppSettingsBranchModeKeepsDefaultPattern(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app := settingsApp(t, env)
	rec := postSettings(t, env, app.ID, url.Values{"deploy_trigger": {"branch"}, "tag_pattern": {""}, "branch": {"main"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	got, _ := env.store.GetApp(context.Background(), app.ID)
	if got.DeployTrigger != core.TriggerBranch || got.TagPattern != "v*" {
		t.Errorf("got trigger %q pattern %q", got.DeployTrigger, got.TagPattern)
	}
}

func TestUpdateAppSettingsRefusesTagModeForTemplatesAndPreviews(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	parent := settingsApp(t, env)
	tmpl, err := env.store.CreateApp(ctx, core.App{
		Name: "tmpl", Source: core.SourceTemplate, TemplateID: "wordpress", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := env.store.CreateApp(ctx, core.App{
		Name: "web-pr-7", RepoURL: parent.RepoURL, Source: core.SourcePublic, Branch: "feature",
		ParentID: parent.ID, PRNumber: 7, Ephemeral: true, PreviewStatus: core.PreviewReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []core.App{tmpl, preview} {
		rec := postSettings(t, env, app.ID, url.Values{
			"deploy_trigger": {"tag"}, "tag_pattern": {"v*"}, "branch": {"changed"},
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "This app can't deploy on tags.") {
			t.Errorf("%s: status = %d body = %q", app.Name, rec.Code, rec.Body.String())
		}
		got, _ := env.store.GetApp(ctx, app.ID)
		if got.DeployTrigger != core.TriggerBranch || got.Branch == "changed" {
			t.Errorf("%s changed: %+v", app.Name, got)
		}
	}
}

func TestAppPageShowsDeploymentTag(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app := settingsApp(t, env)
	dep, _ := env.store.CreateDeployment(context.Background(), app.ID)
	if err := env.store.SetDeploymentTag(context.Background(), dep.ID, "v1.4.0"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/apps/" + itoa(app.ID), "/deployments/" + itoa(dep.ID)} {
		if page := body(t, env.get(t, p)); !strings.Contains(page, "v1.4.0") {
			t.Errorf("%s does not show the tag", p)
		}
	}
}

func TestAppPageTagModeHints(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	pub, err := env.store.CreateApp(ctx, core.App{
		Name: "pub", RepoURL: "https://github.com/o/p.git", Source: core.SourcePublic, Branch: "main",
		WebhookToken: "tok", WebhookSecret: "sek",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, pub.ID, "v*", false, true)
	gh, err := env.store.CreateApp(ctx, core.App{
		Name: "gh", RepoURL: "https://github.com/o/g.git", Source: core.SourceGithub, GithubRepo: "o/g", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, gh.ID, "rel-*", false, true)

	if page := body(t, env.get(t, "/apps/"+itoa(pub.ID))); !strings.Contains(page, "Tag push events") {
		t.Error("public tag-mode app lacks the GitLab note")
	}
	if page := body(t, env.get(t, "/apps/"+itoa(gh.ID))); !strings.Contains(page, "more than three tags") {
		t.Error("GitHub tag-mode app lacks the three-tag note")
	}
}

func TestTagModeShowsTriggerNotBranch(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	tagApp, err := env.store.CreateApp(ctx, core.App{
		Name: "tagged", RepoURL: "https://github.com/o/t.git", Source: core.SourcePublic, Branch: "trunk-xyz",
		WebhookToken: "tok", WebhookSecret: "sek",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, tagApp.ID, "rel-*", false, true)
	// setTagMode saves Branch "main"; give the branch a distinct marker.
	branchApp, err := env.store.CreateApp(ctx, core.App{
		Name: "plain", RepoURL: "https://github.com/o/p.git", Source: core.SourcePublic, Branch: "plainbranch",
		WebhookToken: "tok2", WebhookSecret: "sek2",
	})
	if err != nil {
		t.Fatal(err)
	}

	page := body(t, env.get(t, "/apps/"+itoa(tagApp.ID)))
	if !strings.Contains(page, "<dt>Deploys</dt>") || !strings.Contains(page, "tags matching") || !strings.Contains(page, `<span class="mono">rel-*</span></dd>`) {
		t.Error("tag-mode app page lacks the trigger summary")
	}
	if strings.Contains(page, "<dt>Branch</dt>") {
		t.Error("tag-mode app page still shows a Branch summary row")
	}
	if page := body(t, env.get(t, "/apps/"+itoa(branchApp.ID))); !strings.Contains(page, "<dt>Branch</dt><dd class=\"mono\">plainbranch</dd>") {
		t.Error("branch-mode app page lost its Branch row")
	}

	for _, path := range []string{"/apps", "/projects/" + itoa(tagApp.ProjectID)} {
		list := body(t, env.get(t, path))
		if !strings.Contains(list, "tags: rel-*") {
			t.Errorf("%s lacks the tag trigger", path)
		}
		if !strings.Contains(list, ">plainbranch<") {
			t.Errorf("%s lost the branch-mode branch", path)
		}
		if strings.Contains(list, ">main<") {
			t.Errorf("%s shows the branch for a tag-mode app", path)
		}
	}
}

func TestTagModeCopy(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	pub, err := env.store.CreateApp(ctx, core.App{
		Name: "pub", RepoURL: "https://github.com/o/p.git", Source: core.SourcePublic, Branch: "main",
		WebhookToken: "tok", WebhookSecret: "sek",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, pub.ID, "rel-*", false, true)
	page := body(t, env.get(t, "/apps/"+itoa(pub.ID)))
	if !strings.Contains(page, "more than three tags") || !strings.Contains(page, "GitLab: also tick Tag push events") {
		t.Error("public tag-mode webhook panel lacks the GitHub three-tag note or the GitLab note")
	}
	if strings.Contains(page, "<p class=\"muted env-note\">Watch paths") {
		t.Error("watch paths note must be tied to branch mode")
	}
	if !strings.Contains(page, `data-trigger="branch">Watch paths:`) && !strings.Contains(page, `data-trigger="branch" class="muted env-note">Watch paths`) {
		t.Error("watch paths note lacks data-trigger=branch")
	}

	gh, err := env.store.CreateApp(ctx, core.App{
		Name: "gh", RepoURL: "https://github.com/o/g.git", Source: core.SourceGithub, GithubRepo: "o/g", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, gh.ID, "rel-*", false, true)
	page = body(t, env.get(t, "/apps/"+itoa(gh.ID)))
	if !strings.Contains(page, "new tags matching") || !strings.Contains(page, "when auto-deploy is enabled. No webhook setup needed.") {
		t.Error("GitHub App tag-mode copy wrong")
	}
	if strings.Contains(page, "— New tags") {
		t.Error("capitalisation slip")
	}
}

func TestPrunedImageTextIsModeNeutral(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app := seedRunningApp(t, env, "")
	setTagMode(t, env, app.ID, "v*", false, true)
	dep := seedFinishedDeploy(t, env, app.ID, "outhaul/web:1")
	if err := env.store.MarkImagePruned(context.Background(), dep.Image); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/apps/" + itoa(app.ID), "/deployments/" + itoa(dep.ID)} {
		page := body(t, env.get(t, p))
		if !strings.Contains(page, "image pruned") {
			t.Fatalf("%s lacks the pruned marker", p)
		}
		if strings.Contains(page, "rebuild from the branch") {
			t.Errorf("%s says branch for a tag-mode app", p)
		}
	}
}

func TestTagModePushHintFollowsPattern(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "pushy", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks,
	})
	if err != nil {
		t.Fatal(err)
	}
	setTagMode(t, env, app.ID, "rel-*", false, true)
	page := body(t, env.get(t, "/apps/"+itoa(app.ID)))
	if strings.Contains(page, "git tag v1.0.0") {
		t.Error("push hint hard-codes v1.0.0")
	}
	if !strings.Contains(page, "git tag <tag> && git push outhaul <tag>") || !strings.Contains(page, "a tag matching rel-*") {
		t.Errorf("push hint lacks the pattern-tied placeholder:\n%s", page[strings.Index(page, "git remote add"):][:300])
	}
}
