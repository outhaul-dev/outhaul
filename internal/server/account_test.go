package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

const testKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func TestAccountPageWorksWithNoRole(t *testing.T) {
	e := newTestEnv(t)
	tok := e.signInAs(t, "nobody", core.RoleNone)
	resp := e.getAs(t, tok, "/account")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /account = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, "Change password") {
		t.Error("missing the Change password panel")
	}
	for _, bad := range []string{`href="/apps"`, `href="/projects"`} {
		if strings.Contains(page, bad) {
			t.Errorf("sidebar has %s for an account with no role", bad)
		}
	}
}

func TestPushKeysBelongToTheirOwner(t *testing.T) {
	e := newTestEnv(t)
	a := e.signInAs(t, "alice", core.RoleProjectMember)
	b := e.signInAs(t, "bob", core.RoleProjectMember)
	if got := e.postAs(t, a, "/account/push-keys", url.Values{"label": {"alice-laptop"}, "public_key": {testKeyA}}).StatusCode; got != http.StatusSeeOther {
		t.Fatalf("add key = %d, want 303", got)
	}
	if !strings.Contains(body(t, e.getAs(t, a, "/account")), "alice-laptop") {
		t.Error("alice doesn't see her key")
	}
	if strings.Contains(body(t, e.getAs(t, b, "/account")), "alice-laptop") {
		t.Error("bob sees alice's key")
	}
	keys, _ := e.store.ListPushKeysForUser(context.Background(), userID(t, e, "alice"))
	if len(keys) != 1 {
		t.Fatalf("alice has %d keys, want 1", len(keys))
	}
	e.postAs(t, b, "/account/push-keys/"+itoa(keys[0].ID)+"/delete", nil)
	if keys, _ = e.store.ListPushKeysForUser(context.Background(), userID(t, e, "alice")); len(keys) != 1 {
		t.Error("bob deleted alice's key")
	}
	e.postAs(t, a, "/account/push-keys/"+itoa(keys[0].ID)+"/delete", nil)
	if keys, _ = e.store.ListPushKeysForUser(context.Background(), userID(t, e, "alice")); len(keys) != 0 {
		t.Error("alice couldn't delete her own key")
	}
}

func TestChangePasswordRedirectsToAccount(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.postForm(t, "/account/password", url.Values{"current": {"supersecret"}, "new": {"newpassword1"}})
	if loc := resp.Header.Get("Location"); loc != "/account?ok=1" {
		t.Fatalf("Location = %q, want /account?ok=1", loc)
	}
	if !strings.Contains(body(t, e.get(t, "/account?ok=1")), "Password updated.") {
		t.Error("no confirmation on /account")
	}
}

func TestSettingsNoLongerHasPasswordOrKeys(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	page := body(t, e.get(t, "/settings"))
	for _, gone := range []string{"/settings/password", "/settings/push-keys"} {
		if strings.Contains(page, gone) {
			t.Errorf("settings still has %s", gone)
		}
	}
	for _, p := range []string{"/settings/password", "/settings/push-keys"} {
		got := e.postForm(t, p, url.Values{}).StatusCode
		if got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404 or 405", p, got)
		}
	}
}
