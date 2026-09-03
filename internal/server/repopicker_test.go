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

func installSource(t *testing.T, env *testEnv, appID int64, slug string, installID int64, login, kind string) int64 {
	t.Helper()
	id := connectApp(t, env, appID, slug)
	if err := env.store.BindGithubInstallation(context.Background(), id, installID, login, kind); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	return id
}

func TestAppFormGroupsReposByAccount(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	jsmartID := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	acmeID := installSource(t, env, 77, "outhaul-b", 9002, "acme-corp", "Organization")
	// The fake ignores which source's token it was called with, so both groups
	// list this same repo — which is exactly what lets us tell the groups'
	// options apart: each rendered <option> must carry its own group's id, not
	// a shared or blank one.
	env.gh.Repos = []github.Repo{{FullName: "acme-corp/api", DefaultBranch: "main"}}

	page := body(t, env.get(t, "/apps"))
	if !strings.Contains(page, `<optgroup label="jsmart (personal)"`) {
		t.Error("personal account group missing")
	}
	if !strings.Contains(page, `<optgroup label="acme-corp (org)"`) {
		t.Error("organization group missing")
	}
	if !strings.Contains(page, `name="git_source_id"`) {
		t.Error("hidden git_source_id field missing")
	}
	// Picking a repo also picks its credentials — that only works if the
	// option's data-source-id names its own group's source, not the wrong
	// group's (or nothing, if the {{$group := .}} binding is ever lost).
	wantJsmart := `data-source-id="` + strconv.FormatInt(jsmartID, 10) + `"`
	wantAcme := `data-source-id="` + strconv.FormatInt(acmeID, 10) + `"`
	if !strings.Contains(page, wantJsmart) {
		t.Errorf("no option carries jsmart's own source id (%s)", wantJsmart)
	}
	if !strings.Contains(page, wantAcme) {
		t.Errorf("no option carries acme-corp's own source id (%s)", wantAcme)
	}
}

// One account failing to list must not blank the whole dropdown — which is
// exactly what the old single-slot cache did. The failure is injected with a
// private key AppJWT cannot parse, so only that source's group dies.
func TestRepoGroupsSurviveOneFailingSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	good := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	broken, err := env.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-b", PrivateKey: "not-a-pem",
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := env.store.BindGithubInstallation(ctx, broken.ID, 9002, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	env.gh.Repos = []github.Repo{{FullName: "jsmart/outhaul", DefaultBranch: "main"}}

	data := env.srv.gitSourceData(httptest.NewRequest("GET", "/apps", nil))
	groups, _ := data["RepoGroups"].([]repoGroup)
	var haveGood bool
	for _, g := range groups {
		if g.SourceID == good {
			haveGood = true
		}
		if g.SourceID == broken.ID {
			t.Error("a source whose key cannot mint a JWT must not offer repos")
		}
	}
	if !haveGood {
		t.Error("a failing source must not remove the working source's repos")
	}
	if data["GitSourceConnected"] != true {
		t.Error("GitSourceConnected must stay true while any source is installed")
	}
}

func TestCreateAppRejectsAnUnknownGitSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	form := url.Values{
		"name": {"web"}, "domain": {"web.test"}, "source": {"github"},
		"github_repo": {"jsmart/outhaul"}, "git_source_id": {"4242"},
		"branch": {"main"}, "kind": {"nixpacks"},
	}
	res := env.postForm(t, "/apps", form)
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("app created against a git source that does not exist")
	}
	if _, err := env.store.GetAppByName(context.Background(), "web"); err == nil {
		t.Error("app row created despite the bad source")
	}
}

// TestCreateAppRejectsARepoNotInTheSourcesFreshList exercises resolveRepoSource's
// fresh-cache rejection branch: a real, installed source, with a warm repo
// cache, submitted alongside a repo that isn't in that source's list. This is
// the guard that stops a repo being deployed under another account's
// credentials — TestCreateAppRejectsAnUnknownGitSource only covers the source
// not existing at all, and TestCreateAppStoresTheChosenGitSource runs with a
// cold cache, so neither reaches this branch.
func TestCreateAppRejectsARepoNotInTheSourcesFreshList(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.gh.Repos = []github.Repo{{FullName: "jsmart/real-repo", DefaultBranch: "main"}}

	// Warm the cache via a normal page render so resolveRepoSource takes the
	// fresh-list branch instead of the stale-or-missing "accept" fallback.
	warm := env.get(t, "/apps")
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm GET /apps status = %d", warm.StatusCode)
	}
	body(t, warm)

	form := url.Values{
		"name": {"web"}, "domain": {"web.test"}, "source": {"github"},
		"github_repo": {"someone-else/other-repo"}, "git_source_id": {strconv.FormatInt(id, 10)},
		"branch": {"main"}, "kind": {"nixpacks"},
	}
	res := env.postForm(t, "/apps", form)
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("app created with a repo absent from the source's fresh repo list")
	}
	if _, err := env.store.GetAppByName(context.Background(), "web"); err == nil {
		t.Error("app row created despite the repo not belonging to the chosen source")
	}
}

// TestUpdateAppSourceRejectsARepoNotInTheSourcesFreshList is the same guard on
// handleUpdateAppSource's github branch (the change-source form), which has its
// own call to resolveRepoSource and its own literal source id parse.
func TestUpdateAppSourceRejectsARepoNotInTheSourcesFreshList(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.gh.Repos = []github.Repo{{FullName: "jsmart/real-repo", DefaultBranch: "main"}}

	warm := env.get(t, "/apps")
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm GET /apps status = %d", warm.StatusCode)
	}
	body(t, warm)

	app, err := env.store.CreateApp(context.Background(), core.App{
		Name: "web2", RepoURL: "https://example.com/r.git", Domain: "web2.test",
		Source: core.SourcePublic, Branch: "main", Kind: core.KindNixpacks, WebhookSecret: "w",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	res := env.postForm(t, "/apps/"+strconv.FormatInt(app.ID, 10)+"/source", url.Values{
		"source": {"github"}, "github_repo": {"someone-else/other-repo"},
		"git_source_id": {strconv.FormatInt(id, 10)},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	got, err := env.store.GetApp(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Source == core.SourceGithub {
		t.Error("app source changed despite the repo not belonging to the chosen source")
	}
}

func TestCreateAppStoresTheChosenGitSource(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.gh.Repos = []github.Repo{{FullName: "jsmart/outhaul", DefaultBranch: "main"}}

	form := url.Values{
		"name": {"web"}, "domain": {"web.test"}, "source": {"github"},
		"github_repo": {"jsmart/outhaul"}, "git_source_id": {strconv.FormatInt(id, 10)},
		"branch": {"main"}, "kind": {"nixpacks"},
	}
	if res := env.postForm(t, "/apps", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	app, err := env.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("GetAppByName: %v", err)
	}
	if app.GitSourceID != id {
		t.Errorf("GitSourceID = %d, want %d", app.GitSourceID, id)
	}
	if app.Source != core.SourceGithub {
		t.Errorf("source = %q", app.Source)
	}
}
