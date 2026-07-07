package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestAttachAndDetachDatabase(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	db := seedDatabase(t, e, "web-db", "postgres")

	form := url.Values{"database_id": {itoa(db.ID)}, "env_var": {"DATABASE_URL"}}
	res := e.postForm(t, "/apps/"+itoa(app.ID)+"/attachments", form)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("attach status = %d", res.StatusCode)
	}
	atts, _ := e.store.ListAttachments(context.Background(), app.ID)
	if len(atts) != 1 || atts[0].EnvVar != "DATABASE_URL" {
		t.Fatalf("attachments = %+v", atts)
	}

	// Invalid env var rejected (4xx).
	bad := url.Values{"database_id": {itoa(db.ID)}, "env_var": {"lower case"}}
	if r := e.postForm(t, "/apps/"+itoa(app.ID)+"/attachments", bad); r.StatusCode/100 != 4 {
		r.Body.Close()
		t.Fatalf("bad env var status = %d, want 4xx", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// PORT rejected (4xx) — must not be silently accepted; PORT is managed.
	portForm := url.Values{"database_id": {itoa(db.ID)}, "env_var": {"PORT"}}
	if r := e.postForm(t, "/apps/"+itoa(app.ID)+"/attachments", portForm); r.StatusCode/100 != 4 {
		r.Body.Close()
		t.Fatalf("PORT env var status = %d, want 4xx", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// Detach.
	res = e.postForm(t, "/apps/"+itoa(app.ID)+"/attachments/"+itoa(atts[0].ID)+"/delete", url.Values{})
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("detach status = %d", res.StatusCode)
	}
	atts, _ = e.store.ListAttachments(context.Background(), app.ID)
	if len(atts) != 0 {
		t.Fatalf("expected 0 after detach, got %d", len(atts))
	}
}

func TestAppPageShowsAttachments(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	db := seedDatabase(t, e, "web-db", "postgres")
	if _, err := e.store.AttachDatabase(context.Background(), app.ID, db.ID, "DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	body := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(body, "DATABASE_URL") || !strings.Contains(body, "web-db") {
		t.Fatalf("app page missing attachment; body:\n%s", body)
	}
}
