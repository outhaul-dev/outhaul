package deploy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/config"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
	"github.com/outhaul-dev/outhaul/internal/gitsource"
)

// testKeyPEM generates a PKCS#1 RSA key PEM; AppJWT needs a real key.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// TestCloneSpecUsesTheAppsOwnGitSource seeds TWO distinct sources — bound to
// two different installations, with logins chosen so source A sorts and
// numbers first either way (lower id, alphabetically first) — and gives the
// app the SECOND one's GitSourceID. This deliberately defeats a "grab the
// only/first source" implementation: such a bug would mint against A's
// installation (9000) and this test would catch it, whereas a single-source
// fixture could not distinguish "resolved by id" from "grabbed the only row".
func TestCloneSpecUsesTheAppsOwnGitSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	srcA, err := h.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 76, Slug: "outhaul-a", PrivateKey: testKeyPEM(t),
		WebhookSecret: "whs-a", ClientID: "cid-a", ClientSecret: "csec-a",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource(A): %v", err)
	}
	if err := h.store.BindGithubInstallation(ctx, srcA.ID, 9000, "aaa-org", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation(A): %v", err)
	}

	srcB, err := h.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-b", PrivateKey: testKeyPEM(t),
		WebhookSecret: "whs-b", ClientID: "cid-b", ClientSecret: "csec-b",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource(B): %v", err)
	}
	if err := h.store.BindGithubInstallation(ctx, srcB.ID, 9001, "zzz-org", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation(B): %v", err)
	}

	app := core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "zzz-org/api",
		GitSourceID: srcB.ID, Branch: "main",
	}

	// A worker built with its own visible fake client (rather than
	// h.worker's) so the test can read back which installation the token was
	// minted for.
	gh := &github.Fake{Token: "ghs_test"}
	cfg := config.Config{DataDir: t.TempDir(), Network: "outhaul"}
	w := NewWorker(h.store, h.docker, Builders{Nixpacks: h.builder, Dockerfile: h.dockerfile},
		h.compose, h.cloner, h.broker,
		gitsource.NewRegistry(gitsource.NewGithubApp(gh)), cfg)

	spec, err := w.cloneSpec(ctx, app)
	if err != nil {
		t.Fatalf("cloneSpec: %v", err)
	}
	if spec.URL != "https://github.com/zzz-org/api.git" {
		t.Errorf("URL = %q", spec.URL)
	}
	if spec.Auth.Kind != AuthToken || spec.Auth.Token == "" {
		t.Errorf("auth = %+v, want a token", spec.Auth)
	}
	if gh.LastInstallationID != 9001 {
		t.Errorf("minted token for installation %d, want source B's installation 9001 (app.GitSourceID=%d, not A's %d)",
			gh.LastInstallationID, srcB.ID, srcA.ID)
	}
}

func TestCloneSpecFailsClearlyWithoutASource(t *testing.T) {
	h := newHarness(t)
	app := core.App{Name: "web", Source: core.SourceGithub, GithubRepo: "acme-corp/api", GitSourceID: 0}

	_, err := h.worker.cloneSpec(context.Background(), app)
	if err == nil {
		t.Fatal("an app with no git source must not produce a clone spec")
	}
	if !strings.Contains(err.Error(), "git source") {
		t.Errorf("error = %q, want it to name the missing git source", err)
	}
}
