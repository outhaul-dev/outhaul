package gitsource

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/github"
)

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

func installedSource(t *testing.T) core.GitSource {
	t.Helper()
	return core.GitSource{
		ID: 1, Kind: core.GitSourceGithubApp, AccountLogin: "acme-corp", AccountType: "Organization",
		GithubApp: core.GithubAppCreds{
			AppID: 77, Slug: "outhaul-a", PrivateKey: testKeyPEM(t),
			WebhookSecret: "whs", InstallationID: 9001,
		},
	}
}

func TestGithubAppTokenMintsForTheSourcesInstallation(t *testing.T) {
	f := &github.Fake{Token: "ghs_abc"}
	p := NewGithubApp(f)

	tok, err := p.Token(context.Background(), installedSource(t))
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_abc" {
		t.Errorf("token = %q", tok)
	}
	if f.LastInstallationID != 9001 {
		t.Errorf("minted for installation %d, want 9001", f.LastInstallationID)
	}
}

func TestGithubAppRefusesUninstalledSource(t *testing.T) {
	src := installedSource(t)
	src.GithubApp.InstallationID = 0
	p := NewGithubApp(&github.Fake{Token: "ghs_abc"})

	if _, err := p.Token(context.Background(), src); err == nil {
		t.Error("Token on an uninstalled source must fail")
	}
	if _, err := p.Repos(context.Background(), src); err == nil {
		t.Error("Repos on an uninstalled source must fail")
	}
}

func TestGithubAppReposListsInstallationRepos(t *testing.T) {
	f := &github.Fake{Token: "ghs_abc", Repos: []github.Repo{
		{FullName: "acme-corp/api", DefaultBranch: "main"},
	}}
	p := NewGithubApp(f)

	repos, err := p.Repos(context.Background(), installedSource(t))
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme-corp/api" || repos[0].DefaultBranch != "main" {
		t.Errorf("repos = %+v", repos)
	}
}

func TestGithubAppVerifyWebhookUsesTheSourcesSecret(t *testing.T) {
	src := installedSource(t)
	p := NewGithubApp(&github.Fake{})
	body := []byte(`{"ref":"refs/heads/main"}`)

	mac := hmac.New(sha256.New, []byte("whs"))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !p.VerifyWebhook(src, h, body) {
		t.Error("a body signed with the source's secret must verify")
	}

	other := http.Header{}
	other.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if p.VerifyWebhook(src, other, body) {
		t.Error("a bad signature must not verify")
	}
}

// TestGithubAppVerifyWebhookRejectsWrongKind guards against a source of some
// other Kind ever reaching this provider: without the Kind check,
// src.GithubApp.WebhookSecret would be its zero value (empty), and an empty
// HMAC key verifies anything. Not reachable via the Registry today, but
// appJWT carries the same guard, so VerifyWebhook must too.
func TestGithubAppVerifyWebhookRejectsWrongKind(t *testing.T) {
	src := installedSource(t)
	src.Kind = "something-else"
	p := NewGithubApp(&github.Fake{})
	body := []byte(`{"ref":"refs/heads/main"}`)

	mac := hmac.New(sha256.New, []byte(""))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if p.VerifyWebhook(src, h, body) {
		t.Error("a source of the wrong Kind must not verify, even against an empty secret")
	}
}

func TestRegistryResolvesByKind(t *testing.T) {
	reg := NewRegistry(NewGithubApp(&github.Fake{Token: "t"}))
	if _, err := reg.For(core.GitSourceGithubApp); err != nil {
		t.Fatalf("For(github_app): %v", err)
	}
	if _, err := reg.For("gitlab"); err == nil {
		t.Error("an unknown kind must error, not return a nil provider")
	}
}
