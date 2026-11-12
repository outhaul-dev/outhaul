package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func backoffEnv(t *testing.T) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	e.srv.SetSignInBackoff(true)
	e.srv.signIn.now = func() time.Time { return now }
	return e, &now
}

func postLogin(t *testing.T, e *testEnv, name, password string) (*http.Response, string) {
	t.Helper()
	resp := e.postForm(t, "/login", url.Values{"username": {name}, "password": {password}})
	return resp, body(t, resp)
}

func TestLoginRefusesALockedNameWithTheRightPassword(t *testing.T) {
	e, now := backoffEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")

	for i := 0; i < 5; i++ {
		resp, page := postLogin(t, e, "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "Invalid username or password.") {
			t.Fatalf("wrong attempt %d = %d %q, want 401", i+1, resp.StatusCode, page)
		}
	}

	resp, page := postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the right password during a lock = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if !strings.Contains(page, "Too many failed sign-ins for this username. Try again in less than a minute.") {
		t.Errorf("locked page = %q", page)
	}

	*now = now.Add(31 * time.Second)
	resp, _ = postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after the wait = %d, want 303", resp.StatusCode)
	}
}

func TestLoginLockShowsTheWaitInMinutes(t *testing.T) {
	e, now := backoffEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")

	wrong := func() {
		t.Helper()
		resp, _ := postLogin(t, e, "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt = %d, want 401", resp.StatusCode)
		}
	}
	for i := 0; i < 5; i++ {
		wrong()
	}
	*now = now.Add(31 * time.Second)
	wrong()
	*now = now.Add(61 * time.Second)
	wrong()
	*now = now.Add(121 * time.Second)
	wrong()

	resp, page := postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the right password during a lock = %d, want 429", resp.StatusCode)
	}
	if !strings.Contains(page, "Try again in 4 minutes.") {
		t.Errorf("locked page = %q", page)
	}
	if got := resp.Header.Get("Retry-After"); got != "240" {
		t.Errorf("Retry-After = %q, want 240", got)
	}
}

func TestUnknownUsernameLocksTheSameWay(t *testing.T) {
	e, _ := backoffEnv(t)
	for i := 0; i < 5; i++ {
		resp, _ := postLogin(t, e, "nobody", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp, page := postLogin(t, e, "nobody", "wrong")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("sixth attempt = %d, Retry-After %q; want 429 and 30", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(page, "Too many failed sign-ins for this username. Try again in less than a minute.") {
		t.Errorf("locked page = %q", page)
	}
}

func TestBackoffOffDoesNotLock(t *testing.T) {
	e := newTestEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	for i := 0; i < 6; i++ {
		resp, _ := postLogin(t, e, "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	if e.srv.signIn != nil {
		t.Error("the limiter exists without SetSignInBackoff")
	}
}

func TestBusyHashSlotGivesTheAttemptBack(t *testing.T) {
	e, _ := backoffEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	e.srv.hashWait = 50 * time.Millisecond
	for range cap(e.srv.hashSlots) {
		e.srv.hashSlots <- struct{}{}
	}
	for i := 0; i < 5; i++ {
		resp, _ := postLogin(t, e, "admin", "supersecret")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("busy attempt %d = %d, want 503", i+1, resp.StatusCode)
		}
	}
	for range cap(e.srv.hashSlots) {
		<-e.srv.hashSlots
	}
	resp, _ := postLogin(t, e, "admin", "supersecret")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the right password after the busy attempts = %d, want 303", resp.StatusCode)
	}
}

func TestAccountPasswordCheckIsNotCounted(t *testing.T) {
	e, _ := backoffEnv(t)
	e.login(t)
	for i := 0; i < 6; i++ {
		resp := e.postForm(t, "/account/password", url.Values{
			"current": {"wrong"}, "new": {"newpassword1"},
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("attempt %d = %d, want 400", i+1, resp.StatusCode)
		}
	}
	if n := len(e.srv.signIn.known) + len(e.srv.signIn.unknown); n != 0 {
		t.Errorf("the limiter holds %d names after the Account page's checks, want 0", n)
	}
	resp, _ := postLogin(t, e, "admin", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong /login after them = %d, want 401", resp.StatusCode)
	}
}

func TestStoreFailureCountsAsAnUnknownName(t *testing.T) {
	e, _ := backoffEnv(t)
	e.srv.publicURL = ""
	e.store.Close()
	resp, _ := postLogin(t, e, "admin", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong /login with the store closed = %d, want 401", resp.StatusCode)
	}
	if n := len(e.srv.signIn.unknown); n != 1 {
		t.Errorf("unknown names = %d, want 1", n)
	}
}

func TestPasswordAccountWithoutAPasswordIsKnown(t *testing.T) {
	e, _ := backoffEnv(t)
	if _, err := e.store.CreatePasswordAccount(context.Background(), "bob", core.RoleProjectMember); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		resp, _ := postLogin(t, e, "bob", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	if len(e.srv.signIn.known) != 1 || len(e.srv.signIn.unknown) != 0 {
		t.Fatalf("known %d, unknown %d; want 1 and 0", len(e.srv.signIn.known), len(e.srv.signIn.unknown))
	}
	for i := 0; i < maxUnknownSignInNames+1; i++ {
		e.srv.signIn.allow("made-up-"+strconv.Itoa(i), false)
	}
	if _, ok := e.srv.signIn.allow("bob", true); ok {
		t.Error("a flood of unknown names released bob's lock")
	}
}

func TestGateSignInIsLockedToo(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	e.srv.SetSignInBackoff(true)
	e.srv.signIn.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		resp := e.gateRequest(t, http.MethodPost, "/gate/sign-in", "",
			signInForm("nora", "wrong", "https://wiki.example.com/", testNonce))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong gate attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	resp := e.gateRequest(t, http.MethodPost, "/gate/sign-in", "",
		signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
	page := body(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("locked gate sign-in = %d, Retry-After %q; want 429 and 30", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(page, "Too many failed sign-ins for this username.") {
		t.Errorf("gate locked page = %q", page)
	}
}

func TestSetupLinkClearsTheLock(t *testing.T) {
	e, _ := backoffEnv(t)
	ctx := context.Background()
	u, err := e.store.CreatePasswordAccount(ctx, "bob", core.RoleProjectMember)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		resp, _ := postLogin(t, e, "bob", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	if err := e.store.CreateSetupLink(ctx, u.ID, "setup-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resp := e.postForm(t, "/account/setup", url.Values{
		"token": {"setup-token"}, "password": {"newpassword1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup link = %d, want 303", resp.StatusCode)
	}
	resp, _ = postLogin(t, e, "bob", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after a setup link a wrong password = %d, want 401 (the lock was cleared)", resp.StatusCode)
	}
}
