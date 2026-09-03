package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// postPush delivers a signed push webhook as GitHub would, naming the App.
func postPush(t *testing.T, env *testEnv, appID int64, secret, repo, branch string) *httptest.ResponseRecorder {
	t.Helper()
	payload := `{"ref":"refs/heads/` + branch + `","repository":{"full_name":"` + repo + `"}}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))

	req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", strconv.FormatInt(appID, 10))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The security-relevant case: two connected accounts, the same repo full name
// under each. A push signed by one must not deploy the other's app.
func TestWebhookDeploysOnlyTheSigningSourcesApp(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	srcA := connectApp(t, env, 55, "outhaul-a")
	srcB := connectApp(t, env, 77, "outhaul-b")

	appA, _ := env.store.CreateApp(ctx, core.App{
		Name: "a-web", Domain: "a.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "acme-corp/api", GitSourceID: srcA, Branch: "main", AutoDeploy: true, WebhookSecret: "w1",
	})
	appB, _ := env.store.CreateApp(ctx, core.App{
		Name: "b-web", Domain: "b.test", Source: core.SourceGithub, Kind: core.KindNixpacks,
		GithubRepo: "acme-corp/api", GitSourceID: srcB, Branch: "main", AutoDeploy: true, WebhookSecret: "w2",
	})

	rec := postPush(t, env, 55, "whs-outhaul-a", "acme-corp/api", "main")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if got := deployCount(t, env, appA.ID); got != 1 {
		t.Errorf("source A's app got %d deployments, want 1", got)
	}
	if got := deployCount(t, env, appB.ID); got != 0 {
		t.Errorf("source B's app got %d deployments, want 0 — a push for one account must not deploy another's", got)
	}
}

func TestWebhookRejectsAnUnknownApp(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	rec := postPush(t, env, 4242, "whs-outhaul-a", "acme-corp/api", "main")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an unknown App id", rec.Code)
	}
}

func TestWebhookRejectsASignatureFromAnotherSource(t *testing.T) {
	env := newTestEnv(t)
	connectApp(t, env, 55, "outhaul-a")
	connectApp(t, env, 77, "outhaul-b")
	// Claims App 55, signed with App 77's secret.
	rec := postPush(t, env, 55, "whs-outhaul-b", "acme-corp/api", "main")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — a signature from another source must not verify", rec.Code)
	}
}
