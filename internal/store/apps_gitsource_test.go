package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestAppRoundTripsGitSourceID(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))

	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.GitSourceID != src.ID {
		t.Errorf("GitSourceID = %d, want %d", got.GitSourceID, src.ID)
	}
}

// Two accounts can each be connected, and nothing stops the same repo full name
// existing under both. A push for one must never reach the other's app.
func TestAppsByGithubRepoSourceScopesToOneSource(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	a, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	b, _ := s.CreateGithubAppSource(ctx, testCreds(77, "outhaul-b"))

	appA, _ := s.CreateApp(ctx, core.App{
		Name: "a-web", Domain: "a.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: a.ID, WebhookSecret: "w1",
	})
	s.CreateApp(ctx, core.App{
		Name: "b-web", Domain: "b.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: b.ID, WebhookSecret: "w2",
	})

	got, err := s.AppsByGithubRepoSource(ctx, a.ID, "acme-corp/api")
	if err != nil {
		t.Fatalf("AppsByGithubRepoSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d apps, want only source A's", len(got))
	}
	if got[0].ID != appA.ID {
		t.Errorf("matched app %d, want %d", got[0].ID, appA.ID)
	}
}

func TestAppsUsingGitSource(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})
	s.CreateApp(ctx, core.App{
		Name: "plain", Domain: "plain.test", Source: core.SourcePublic,
		RepoURL: "https://example.com/r.git", WebhookSecret: "w2",
	})

	users, err := s.AppsUsingGitSource(ctx, src.ID)
	if err != nil {
		t.Fatalf("AppsUsingGitSource: %v", err)
	}
	if len(users) != 1 || users[0].Name != "web" {
		t.Fatalf("got %d apps (%v), want just web", len(users), users)
	}
	none, _ := s.AppsUsingGitSource(ctx, 4242)
	if len(none) != 0 {
		t.Errorf("unreferenced source reported %d apps", len(none))
	}
}

func TestUpdateAppSourceClearsGitSourceForNonGithub(t *testing.T) {
	s := newSealedStore(t)
	ctx := context.Background()
	src, _ := s.CreateGithubAppSource(ctx, testCreds(55, "outhaul-a"))
	app, _ := s.CreateApp(ctx, core.App{
		Name: "web", Domain: "web.test", Source: core.SourceGithub,
		GithubRepo: "acme-corp/api", GitSourceID: src.ID, WebhookSecret: "w",
	})

	if err := s.UpdateAppSource(ctx, app.ID, core.SourcePublic,
		"https://example.com/r.git", "", 0, "", ""); err != nil {
		t.Fatalf("UpdateAppSource: %v", err)
	}
	got, _ := s.GetApp(ctx, app.ID)
	if got.GitSourceID != 0 {
		t.Errorf("GitSourceID = %d after moving off GitHub, want 0", got.GitSourceID)
	}
}
