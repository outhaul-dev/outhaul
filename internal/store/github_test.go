package store

import (
	"context"
	"testing"

	"github.com/slipwaydev/slipway/internal/core"
)

func TestGithubAppRoundTripEncrypted(t *testing.T) {
	st := gitTestStore(t)
	ctx := context.Background()

	// Not configured initially.
	if _, ok, err := st.GithubApp(ctx); err != nil || ok {
		t.Fatalf("GithubApp before setup: ok=%v err=%v", ok, err)
	}

	in := core.GithubApp{
		AppID: 100, Slug: "slipway-z", PrivateKey: "PEM", WebhookSecret: "whs",
		ClientID: "cid", ClientSecret: "csec",
	}
	if err := st.SetGithubApp(ctx, in); err != nil {
		t.Fatalf("SetGithubApp: %v", err)
	}

	got, ok, err := st.GithubApp(ctx)
	if err != nil || !ok {
		t.Fatalf("GithubApp after setup: ok=%v err=%v", ok, err)
	}
	if got.AppID != 100 || got.Slug != "slipway-z" || got.PrivateKey != "PEM" ||
		got.WebhookSecret != "whs" || got.ClientID != "cid" || got.ClientSecret != "csec" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Secrets are ciphertext on disk.
	var pk string
	if err := st.db.QueryRowContext(ctx, `SELECT private_key FROM github_app WHERE id = 1`).Scan(&pk); err != nil {
		t.Fatal(err)
	}
	if pk == "PEM" {
		t.Error("private_key stored in plaintext")
	}

	// Setting again replaces the single row (no duplicate).
	if err := st.SetGithubApp(ctx, in); err != nil {
		t.Fatalf("SetGithubApp replace: %v", err)
	}
	var n int
	st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM github_app`).Scan(&n)
	if n != 1 {
		t.Errorf("github_app row count = %d, want 1", n)
	}

	// Installation id updates in place.
	if err := st.SetInstallationID(ctx, 777); err != nil {
		t.Fatalf("SetInstallationID: %v", err)
	}
	got, _, _ = st.GithubApp(ctx)
	if got.InstallationID != 777 {
		t.Errorf("InstallationID = %d, want 777", got.InstallationID)
	}

	// Re-running SetGithubApp (e.g. manifest flow run again) must NOT wipe the
	// previously-recorded installation id.
	if err := st.SetGithubApp(ctx, in); err != nil {
		t.Fatalf("SetGithubApp re-run: %v", err)
	}
	got, _, _ = st.GithubApp(ctx)
	if got.InstallationID != 777 {
		t.Errorf("installation id after re-run = %d, want 777 (must be preserved)", got.InstallationID)
	}
}
