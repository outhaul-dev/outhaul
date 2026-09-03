package deploy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
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

func TestCloneSpecUsesTheAppsOwnGitSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	src, err := h.store.CreateGithubAppSource(ctx, core.GithubAppCreds{
		AppID: 77, Slug: "outhaul-b", PrivateKey: testKeyPEM(t),
		WebhookSecret: "whs", ClientID: "cid", ClientSecret: "csec",
	})
	if err != nil {
		t.Fatalf("CreateGithubAppSource: %v", err)
	}
	if err := h.store.BindGithubInstallation(ctx, src.ID, 9001, "acme-corp", "Organization"); err != nil {
		t.Fatalf("BindGithubInstallation: %v", err)
	}
	app := core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "acme-corp/api",
		GitSourceID: src.ID, Branch: "main",
	}

	spec, err := h.worker.cloneSpec(ctx, app)
	if err != nil {
		t.Fatalf("cloneSpec: %v", err)
	}
	if spec.URL != "https://github.com/acme-corp/api.git" {
		t.Errorf("URL = %q", spec.URL)
	}
	if spec.Auth.Kind != AuthToken || spec.Auth.Token == "" {
		t.Errorf("auth = %+v, want a token", spec.Auth)
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
