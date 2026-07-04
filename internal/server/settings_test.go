package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
