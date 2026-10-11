package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestSettingsPageRenders(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	page := body(t, e.get(t, "/settings"))
	if !strings.Contains(page, "Settings") {
		t.Error("settings page missing heading")
	}
	if !strings.Contains(page, "Change password") {
		t.Error("settings page missing password section")
	}
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.postForm(t, "/settings/password", url.Values{
		"current": {"wrong"}, "new": {"newpassword1"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for wrong current password", resp.StatusCode)
	}
}

func TestSettingsShowsPushKeysPanel(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	page := body(t, e.get(t, "/settings"))
	if !strings.Contains(page, "Push keys") {
		t.Error("settings page missing push keys panel")
	}
	if !strings.Contains(page, "Git-push SSH port") {
		t.Error("settings page missing git-push SSH port panel")
	}
	if !strings.Contains(page, "not running") {
		t.Error("settings page should report the SSH server as not running in tests")
	}
}

func TestChangePasswordUpdates(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.postForm(t, "/settings/password", url.Values{
		"current": {"supersecret"}, "new": {"newpassword1"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 after successful change", resp.StatusCode)
	}
	u, _ := e.store.GetUserByName(context.Background(), "admin")
	ok, _ := VerifyPassword(u.PasswordHash, "newpassword1")
	if !ok {
		t.Error("new password hash does not verify")
	}
}

// Changing the password is what an admin does after a session leaks, but
// every existing session stayed valid for its full week. Other sessions are
// revoked now; the one making the change stays signed in.
func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	ctx := context.Background()
	u, _ := e.store.GetUserByName(ctx, "admin")
	if err := e.store.CreateSession(ctx, core.Session{Token: "stolen", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	resp := e.postForm(t, "/settings/password", url.Values{
		"current": {"supersecret"}, "new": {"newpassword1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	if _, err := e.store.GetSession(ctx, "stolen"); err == nil {
		t.Error("another session survived the password change")
	}
	if _, err := e.store.GetSession(ctx, e.sessionToken); err != nil {
		t.Errorf("the session that changed the password was revoked: %v", err)
	}
}
