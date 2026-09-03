package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

func TestSettingsListsEveryConnectedAccount(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	installSource(t, env, 77, "outhaul-b", 9002, "acme-corp", "Organization")

	page := body(t, env.get(t, "/settings"))
	for _, want := range []string{"jsmart", "acme-corp", "Connect another account"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestRemovingAReferencedSourceIsRefused(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")
	env.store.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "jsmart/outhaul", GitSourceID: id, WebhookSecret: "w",
	})

	res := env.postForm(t, "/settings/git-sources/"+strconv.FormatInt(id, 10)+"/delete", url.Values{})
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("removed a source that apps still use")
	}
	if _, ok, _ := env.store.GetGitSource(ctx, id); !ok {
		t.Fatal("source was deleted despite being referenced")
	}
	if page := body(t, res); !strings.Contains(page, "web") {
		t.Error("the refusal must name the apps that depend on the source")
	}
}

func TestRemovingAnUnreferencedSourceSucceeds(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := installSource(t, env, 55, "outhaul-a", 9001, "jsmart", "User")

	env.postForm(t, "/settings/git-sources/"+strconv.FormatInt(id, 10)+"/delete", url.Values{})
	if _, ok, _ := env.store.GetGitSource(context.Background(), id); ok {
		t.Error("source still present after removing it")
	}
}

// A source migrated from the pre-0022 record has no account name; the settings
// page is where we finally ask GitHub for one.
func TestSettingsBackfillsAMissingAccountName(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	id := connectApp(t, env, 55, "outhaul-a")
	if err := env.store.BindGithubInstallation(ctx, id, 9001, "", ""); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	env.gh.InstallationsByApp = map[int64][]github.Installation{
		55: {{ID: 9001, AccountLogin: "jsmart", AccountType: "User"}},
	}

	body(t, env.get(t, "/settings"))

	src, _, _ := env.store.GetGitSource(ctx, id)
	if src.AccountLogin != "jsmart" {
		t.Errorf("account login = %q, want it backfilled to jsmart", src.AccountLogin)
	}
}

// A source whose credentials are permanently broken (revoked key, App
// uninstalled on GitHub's side) must only be probed once per process
// lifetime — renderSettings backs every /settings/... error redisplay too,
// not just GET /settings, and nothing must retry a live GitHub call forever.
func TestBackfillGivesUpAfterOneFailure(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	ctx := context.Background()
	id := connectApp(t, env, 55, "outhaul-a")
	if err := env.store.BindGithubInstallation(ctx, id, 9001, "", ""); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	env.gh.InstallationErr = fmt.Errorf("installation revoked")

	body(t, env.get(t, "/settings"))
	body(t, env.get(t, "/settings"))

	if env.gh.InstallationCalls != 1 {
		t.Errorf("Installation calls = %d, want 1 (probed once, not retried every render)", env.gh.InstallationCalls)
	}
	src, _, _ := env.store.GetGitSource(ctx, id)
	if src.AccountLogin != "" {
		t.Errorf("account login = %q, want still empty after a permanent failure", src.AccountLogin)
	}
}
