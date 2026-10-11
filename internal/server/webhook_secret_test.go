package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCreateAppGetsSeparateWebhookTokenAndSecret(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	resp := e.postForm(t, "/apps", url.Values{
		"name": {"web"}, "repo_url": {"https://example.com/web.git"}, "domain": {"web.example.com"},
	})
	resp.Body.Close()
	app, err := e.store.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("app not created (status %d): %v", resp.StatusCode, err)
	}
	if app.WebhookToken == "" || app.WebhookSecret == "" || app.WebhookToken == app.WebhookSecret {
		t.Fatalf("token=%q secret=%q, want two distinct non-empty values", app.WebhookToken, app.WebhookSecret)
	}
}

// Until now the page showed only the URL, so a sender configured from it was
// always refused: the secret it needed was never displayed.
func TestAppPageShowsWebhookSecret(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://x/y.git", Domain: "web.test", Source: core.SourcePublic,
		WebhookToken: "tok123", WebhookSecret: "sek456",
	})
	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	for _, want := range []string{"/webhooks/app/tok123", "<code>sek456</code>", "<span class=\"mono\">application/json</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("app page does not show %q", want)
		}
	}
}

func TestAppPageOmitsWebhookWithoutToken(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{
		Name: "web", RepoURL: "https://x/y.git", Domain: "web.test", Source: core.SourcePublic,
	})
	if page := body(t, e.get(t, "/apps/"+itoa(app.ID))); strings.Contains(page, "/webhooks/app/") {
		t.Error("app page shows a webhook URL for an app that has no webhook token")
	}
}

func TestRegenerateWebhookSecret(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	ctx := context.Background()
	app, _ := e.store.CreateApp(ctx, core.App{
		Name: "web", RepoURL: "https://x/y.git", Domain: "web.test", Source: core.SourcePublic,
		WebhookToken: "tok123", WebhookSecret: "tok123", // a migrated app: secret == token
	})
	resp := e.postForm(t, "/apps/"+itoa(app.ID)+"/webhook/secret", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, _ := e.store.GetApp(ctx, app.ID)
	if got.WebhookToken != "tok123" {
		t.Errorf("token changed to %q; the sender's URL must keep working", got.WebhookToken)
	}
	if got.WebhookSecret == "" || got.WebhookSecret == "tok123" {
		t.Errorf("secret = %q, want a fresh value distinct from the token", got.WebhookSecret)
	}
}
