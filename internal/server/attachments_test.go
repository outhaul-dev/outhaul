package server

import (
	"bytes"
	"context"
	"log"
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

// A database that can't be read still answers like a missing one, so nothing
// about it is disclosed, but the failure is logged. A missing one isn't.
func TestAttachLogsAStoreFailure(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	db := seedDatabase(t, e, "web-db", "postgres")
	if _, err := e.rawDB(t).Exec(`UPDATE databases SET created_at = 'not a time' WHERE id = ?`, db.ID); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	attach := func(dbID int64) *http.Response {
		return e.postForm(t, "/apps/"+itoa(app.ID)+"/attachments", url.Values{"database_id": {itoa(dbID)}, "env_var": {"DATABASE_URL"}})
	}
	resp := attach(9999)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || logged.Len() != 0 {
		t.Errorf("missing database = %d, logged %q; want 404 and no log", resp.StatusCode, logged.String())
	}
	resp = attach(db.ID)
	page := body(t, resp)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(page, "not a time") {
		t.Errorf("unreadable database = %d %q, want 404 without the error", resp.StatusCode, page)
	}
	if !strings.Contains(logged.String(), "not a time") {
		t.Errorf("the store failure wasn't logged: %q", logged.String())
	}
}
