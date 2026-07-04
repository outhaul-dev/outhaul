package store

import (
	"context"
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
	"github.com/slipwaydev/slipway/internal/secret"
)

func gitTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(dir + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir+"/slipway.db", box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCreateAppPersistsSourceFields(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	in := core.App{
		Name: "web", RepoURL: "https://github.com/o/r.git", Domain: "web.example.com",
		Branch: "release", AutoDeploy: true, Source: core.SourceGithub,
		WebhookSecret: "whsecret", GithubRepo: "o/r",
		SSHPublicKey: "ssh-ed25519 AAAA...", SSHPrivateKey: "PRIVATEKEYPEM",
	}
	created, err := st.CreateApp(ctx, in)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	got, err := st.GetApp(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Branch != "release" || !got.AutoDeploy || got.Source != core.SourceGithub ||
		got.WebhookSecret != "whsecret" || got.GithubRepo != "o/r" || got.SSHPublicKey != "ssh-ed25519 AAAA..." {
		t.Errorf("read back wrong fields: %+v", got)
	}
	// The private key must never be populated on reads.
	if got.SSHPrivateKey != "" {
		t.Errorf("GetApp leaked SSHPrivateKey = %q", got.SSHPrivateKey)
	}
}

func TestSSHPrivateKeyRoundTripEncrypted(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{
		Name: "svc", RepoURL: "git@github.com:o/r.git", Domain: "svc.example.com",
		Source: core.SourceSSH, SSHPrivateKey: "SECRET-PEM", SSHPublicKey: "ssh-ed25519 AAAA",
		Branch: "main", WebhookSecret: "w",
	})
	if err != nil {
		t.Fatal(err)
	}

	key, err := st.SSHPrivateKey(ctx, app.ID)
	if err != nil {
		t.Fatalf("SSHPrivateKey: %v", err)
	}
	if key != "SECRET-PEM" {
		t.Errorf("decrypted key = %q, want SECRET-PEM", key)
	}

	// On disk the value must be ciphertext, not the plaintext PEM.
	var stored string
	if err := st.db.QueryRowContext(ctx, `SELECT ssh_private_key FROM apps WHERE id = ?`, app.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "SECRET-PEM" || stored == "" {
		t.Errorf("ssh_private_key not encrypted at rest: %q", stored)
	}
}

func TestAppLookupsByWebhookSecretAndRepo(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()
	_, err := st.CreateApp(ctx, core.App{
		Name: "gh", RepoURL: "https://github.com/o/r.git", Domain: "gh.example.com",
		Source: core.SourceGithub, GithubRepo: "o/r", Branch: "main",
		AutoDeploy: true, WebhookSecret: "tok123",
	})
	if err != nil {
		t.Fatal(err)
	}

	byTok, err := st.AppByWebhookSecret(ctx, "tok123")
	if err != nil {
		t.Fatalf("AppByWebhookSecret: %v", err)
	}
	if byTok.Name != "gh" {
		t.Errorf("byTok = %+v", byTok)
	}
	if _, err := st.AppByWebhookSecret(ctx, "nope"); err == nil {
		t.Error("expected error for unknown token")
	}

	byRepo, err := st.AppsByGithubRepo(ctx, "o/r")
	if err != nil {
		t.Fatalf("AppsByGithubRepo: %v", err)
	}
	if len(byRepo) != 1 || byRepo[0].Name != "gh" {
		t.Errorf("byRepo = %+v", byRepo)
	}
}

func TestUpdateAppSettings(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()
	app, _ := st.CreateApp(ctx, core.App{
		Name: "a", RepoURL: "https://github.com/o/r.git", Domain: "a.example.com",
		Branch: "main", Source: core.SourcePublic, WebhookSecret: "w",
	})
	if err := st.UpdateAppSettings(ctx, app.ID, "develop", true, []string{"src/**", "package.json"}); err != nil {
		t.Fatalf("UpdateAppSettings: %v", err)
	}
	got, _ := st.GetApp(ctx, app.ID)
	if got.Branch != "develop" || !got.AutoDeploy {
		t.Errorf("settings not updated: %+v", got)
	}
	if len(got.WatchPaths) != 2 || got.WatchPaths[0] != "src/**" || got.WatchPaths[1] != "package.json" {
		t.Errorf("watch paths not round-tripped: %v", got.WatchPaths)
	}

	if err := st.UpdateAppSettings(ctx, app.ID, "develop", true, nil); err != nil {
		t.Fatalf("UpdateAppSettings (clear watch paths): %v", err)
	}
	got, _ = st.GetApp(ctx, app.ID)
	if len(got.WatchPaths) != 0 {
		t.Errorf("watch paths not cleared: %v", got.WatchPaths)
	}
}
