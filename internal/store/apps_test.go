package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestUpdateAppSource(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Source: core.SourcePublic, RepoURL: "https://example.com/r.git",
		Kind: core.KindNixpacks, Domain: "web.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// public -> github
	if err := s.UpdateAppSource(ctx, app.ID, core.SourceGithub,
		"https://github.com/o/n.git", "o/n", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != core.SourceGithub || got.GithubRepo != "o/n" ||
		got.RepoURL != "https://github.com/o/n.git" {
		t.Fatalf("source not updated: %+v", got)
	}
	// github -> ssh, storing a deploy key
	if err := s.UpdateAppSource(ctx, app.ID, core.SourceSSH,
		"git@github.com:o/n.git", "", "ssh-ed25519 AAA deploy", "PRIVATEKEY"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetApp(ctx, app.ID)
	if got.Source != core.SourceSSH || got.SSHPublicKey != "ssh-ed25519 AAA deploy" {
		t.Fatalf("ssh source not updated: %+v", got)
	}
	if got.SSHPrivateKey != "" {
		t.Fatalf("GetApp must never populate the private key, got %q", got.SSHPrivateKey)
	}
}

func TestUpdateAppKind(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	app, err := s.CreateApp(ctx, core.App{
		Name: "web", Source: core.SourcePublic, RepoURL: "https://example.com/r.git",
		Kind: core.KindNixpacks, Domain: "web.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppKind(ctx, app.ID, core.KindCompose, "stack/docker-compose.yml", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetApp(ctx, app.ID)
	if got.Kind != core.KindCompose || got.ComposePath != "stack/docker-compose.yml" {
		t.Fatalf("kind not updated: %+v", got)
	}
}
