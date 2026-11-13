package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sso"
)

var pushTokenRE = regexp.MustCompile(`ohp_[0-9a-f]{64}`)

func newGitHTTPEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, func(s *Server) {
		s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
	})
}

func accountID(t *testing.T, e *testEnv, name string) int64 {
	t.Helper()
	u, err := e.store.GetUserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func tokenCount(t *testing.T, e *testEnv, uid int64) int {
	t.Helper()
	list, err := e.store.ListPushTokensForUser(context.Background(), uid, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestPushTokenRoutesAbsentWhenOff(t *testing.T) {
	e := newTestEnv(t)
	tok := e.signInAs(t, "root", core.RoleSystemAdmin)
	resp := e.postAs(t, tok, "/account/push-tokens", url.Values{"label": {"a"}, "days": {"7"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(body(t, e.getAs(t, tok, "/account")), "Push tokens") {
		t.Error("account page mentions Push tokens while off")
	}
}

func TestCreatePushTokenShowsItOnce(t *testing.T) {
	e := newGitHTTPEnv(t)
	tok := e.signInAs(t, "root", core.RoleSystemAdmin)
	resp := e.postAs(t, tok, "/account/push-tokens", url.Values{"label": {"laptop"}, "days": {"7"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	html := body(t, resp)
	found := pushTokenRE.FindAllString(html, -1)
	if len(found) != 1 {
		t.Fatalf("found %d tokens in the page", len(found))
	}
	if !strings.Contains(html, "It won&#39;t be shown again") && !strings.Contains(html, "It won't be shown again") {
		t.Error("page lacks the shown-once warning")
	}
	pt, ok, err := e.store.FindPushToken(context.Background(), found[0])
	if err != nil || !ok {
		t.Fatalf("FindPushToken: ok=%v err=%v", ok, err)
	}
	if pt.UserID != accountID(t, e, "root") {
		t.Errorf("owner = %d", pt.UserID)
	}
	if d := time.Until(pt.ExpiresAt) - 7*24*time.Hour; d < -time.Minute || d > time.Minute {
		t.Errorf("expires %v from now, want 7 days", time.Until(pt.ExpiresAt))
	}
	page := body(t, e.getAs(t, tok, "/account"))
	if !strings.Contains(page, "laptop") || strings.Contains(page, "ohp_") {
		t.Errorf("account page should list laptop and hold no token value")
	}
}

func TestCreatePushTokenRefusals(t *testing.T) {
	e := newGitHTTPEnv(t)
	tok := e.signInAs(t, "root", core.RoleSystemAdmin)
	uid := accountID(t, e, "root")
	ctx := context.Background()
	now := time.Now()
	if _, _, err := e.store.CreatePushToken(ctx, uid, "taken", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ label, days string }{
		{"", "7"}, {strings.Repeat("a", 65), "7"}, {"a\tb", "7"}, {"taken", "7"},
		{"ok", "0"}, {"ok", "31"}, {"ok", "abc"}, {"ok", ""},
	}
	for _, c := range cases {
		resp := e.postAs(t, tok, "/account/push-tokens", url.Values{"label": {c.label}, "days": {c.days}})
		html := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("label %q days %q: status %d", c.label, c.days, resp.StatusCode)
		}
		if !strings.Contains(html, "alert-error") || pushTokenRE.MatchString(html) {
			t.Errorf("label %q days %q: want an error page with no token", c.label, c.days)
		}
	}
	if n := tokenCount(t, e, uid); n != 1 {
		t.Fatalf("token count = %d, want 1", n)
	}
	for i := 1; i < core.MaxPushTokensPerUser; i++ {
		if _, _, err := e.store.CreatePushToken(ctx, uid, "t"+itoa(int64(i)), now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	resp := e.postAs(t, tok, "/account/push-tokens", url.Values{"label": {"one-more"}, "days": {"7"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("26th token: status %d, want 400", resp.StatusCode)
	}
}

func TestDeleteOwnPushToken(t *testing.T) {
	e := newGitHTTPEnv(t)
	a := e.signInAs(t, "alice", core.RoleSystemAdmin)
	b := e.signInAs(t, "bob", core.RoleSystemAdmin)
	aid := accountID(t, e, "alice")
	now := time.Now()
	_, pt, err := e.store.CreatePushToken(context.Background(), aid, "x", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	path := "/account/push-tokens/" + itoa(pt.ID) + "/delete"
	resp := e.postAs(t, b, path, nil)
	if resp.StatusCode != http.StatusSeeOther || tokenCount(t, e, aid) != 1 {
		t.Fatalf("other account: status %d, count %d", resp.StatusCode, tokenCount(t, e, aid))
	}
	resp = e.postAs(t, a, path, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("owner: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := tokenCount(t, e, aid); n != 0 {
		t.Errorf("count = %d after delete", n)
	}
}

func TestAccountPagePushTokens(t *testing.T) {
	e := newGitHTTPEnv(t)
	tok := e.signInAs(t, "root", core.RoleSystemAdmin)
	uid := accountID(t, e, "root")
	ctx := context.Background()
	now := time.Now()
	if _, _, err := e.store.CreatePushToken(ctx, uid, "old", now.Add(-48*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, used, err := e.store.CreatePushToken(ctx, uid, "busy", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.TouchPushToken(ctx, used.ID, now, "198.51.100.9"); err != nil {
		t.Fatal(err)
	}
	// A password account with a sign-in time must not get the SSO line.
	if _, err := e.rawDB(t).Exec(`UPDATE users SET last_sign_in_at = ? WHERE id = ?`, now.UTC().Format(time.RFC3339Nano), uid); err != nil {
		t.Fatal(err)
	}
	page := body(t, e.getAs(t, tok, "/account"))
	for _, want := range []string{"expired", "198.51.100.9"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Your tokens work until") {
		t.Error("a password account sees the SSO line")
	}

	sso := ssoAccount(t, e, "sam@example.com", core.RoleSystemAdmin)
	var sid int64
	if err := e.rawDB(t).QueryRow(`SELECT id FROM users WHERE email = ?`, "sam@example.com").Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rawDB(t).Exec(`UPDATE users SET last_sign_in_at = ? WHERE id = ?`, now.UTC().Format(time.RFC3339Nano), sid); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(t, e.getAs(t, sso, "/account")), "Your tokens work until") {
		t.Error("an SSO account lacks the line")
	}
}

func TestUsersPagePushTokens(t *testing.T) {
	e := newGitHTTPEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	padmin := e.signInAs(t, "pa", core.RoleProjectAdmin)
	e.signInAs(t, "carol", core.RoleProjectMember)
	e.signInAs(t, "dave", core.RoleProjectMember)
	cid, did := accountID(t, e, "carol"), accountID(t, e, "dave")
	now := time.Now()
	ctx := context.Background()
	_, ct, err := e.store.CreatePushToken(ctx, cid, "carols-ci", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, e.getAs(t, admin, "/users"))
	if !strings.Contains(page, "carol") || !strings.Contains(page, "carols-ci") || strings.Contains(page, "ohp_") {
		t.Error("users page should list the token by owner and label, with no value")
	}
	path := "/users/" + itoa(cid) + "/push-tokens/" + itoa(ct.ID) + "/delete"
	if resp := e.postAs(t, padmin, path, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("project admin: %d, want 403", resp.StatusCode)
	}
	if resp := e.postAs(t, admin, "/users/"+itoa(did)+"/push-tokens/"+itoa(ct.ID)+"/delete", nil); resp.StatusCode != http.StatusSeeOther || tokenCount(t, e, cid) != 1 {
		t.Errorf("mismatched user must delete nothing")
	}
	resp := e.postAs(t, admin, path, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/users" || tokenCount(t, e, cid) != 0 {
		t.Errorf("admin delete: status %d to %q, count %d", resp.StatusCode, resp.Header.Get("Location"), tokenCount(t, e, cid))
	}
}

// ssoTextEnv is a signed-in system admin with SSO saved, and git over HTTPS on
// or off.
func ssoTextEnv(t *testing.T, on bool) *testEnv {
	t.Helper()
	e := newTestEnvWith(t, func(s *Server) {
		if on {
			s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
		}
	})
	e.completeSetup(t)
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) { return &sso.Fake{}, nil }
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	return e
}

// Ending every SSO sign-in deletes SSO accounts' push tokens, so with git over
// HTTPS on the Settings page says so wherever it says sign-ins end. Off, every
// text is as it was.
func TestSSOTextsSayPushTokensAreDeleted(t *testing.T) {
	const (
		removeOff = "Remove single sign-on? Everyone signed in with SSO is signed out. Password sign-in keeps working."
		removeOn  = "Remove single sign-on? Everyone signed in with SSO is signed out and their push tokens are deleted. Password sign-in keeps working."
		changeOff = "Changing the issuer or client ID ends every SSO sign-in; replacing only the secret does not.</p>"
		changeOn  = "Changing the issuer or client ID ends every SSO sign-in and deletes SSO accounts' push tokens; replacing only the secret does not.</p>"
		issuerOff = "The old accounts keep their roles and push keys until they are removed.</div>"
		issuerOn  = "The old accounts keep their roles and push keys until they are removed. Their push tokens are deleted.</div>"
	)
	for _, on := range []bool{false, true} {
		e := ssoTextEnv(t, on)
		panel := body(t, e.get(t, "/settings"))
		issuer := body(t, saveSSO(t, e, "https://other.example.com", "cid", ""))
		e.srv.publicURL = ""
		noURL := body(t, e.get(t, "/settings"))
		pages := []struct {
			name, page, off, on string
		}{
			{"the remove confirm", panel, removeOff, removeOn},
			{"the client ID note", panel, changeOff, changeOn},
			{"the issuer warning", issuer, issuerOff, issuerOn},
			{"the remove confirm without a public URL", noURL, removeOff, removeOn},
		}
		for _, p := range pages {
			want, not := p.off, p.on
			if on {
				want, not = p.on, p.off
			}
			if !strings.Contains(p.page, want) || strings.Contains(p.page, not) {
				t.Errorf("git over HTTPS on=%v: %s isn't %q", on, p.name, want)
			}
		}
		if !on {
			for _, page := range []string{panel, issuer, noURL} {
				if strings.Contains(page, "push token") {
					t.Error("a Settings page names push tokens with git over HTTPS off")
				}
			}
		}
	}
}

// A reset is often for a suspected compromise, and leaves tokens alone, so
// with git over HTTPS on its confirm says so. Off, the Users page is as it
// was, blank lines included.
func TestUsersPageResetConfirmNamesPushTokens(t *testing.T) {
	const (
		resetOff = "Their password stops working and they are signed out now. You will get a new setup link to send them.')"
		resetOn  = "Their password stops working and they are signed out now. You will get a new setup link to send them. Their push tokens keep working until you revoke them below.')"
	)
	for _, on := range []bool{false, true} {
		e := newTestEnvWith(t, func(s *Server) {
			if on {
				s.SetGitHTTP(GitHTTPOptions{DefaultDays: 7, MaxDays: 30, SSOMaxAge: 7 * 24 * time.Hour})
			}
		})
		admin := e.signInAs(t, "root", core.RoleSystemAdmin)
		e.signInAs(t, "carol", core.RoleProjectMember)
		page := body(t, e.getAs(t, admin, "/users"))
		want, not := resetOff, resetOn
		if on {
			want, not = resetOn, resetOff
		}
		if !strings.Contains(page, want) || strings.Contains(page, not) {
			t.Errorf("git over HTTPS on=%v: the reset confirm isn't %q", on, want)
		}
		if !on && !strings.Contains(page, "</section>\n\n<dialog id=\"user-dialog\"") {
			t.Error("with git over HTTPS off, the users table's section isn't followed by one blank line and the dialog")
		}
	}
}

func TestViewerSeesNoTokenValueElsewhere(t *testing.T) {
	e := newGitHTTPEnv(t)
	tok := e.signInAs(t, "root", core.RoleSystemAdmin)
	resp := e.postAs(t, tok, "/account/push-tokens", url.Values{"label": {"laptop"}, "days": {"7"}})
	raw := pushTokenRE.FindString(body(t, resp))
	if raw == "" {
		t.Fatal("no token shown")
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	var stored string
	if err := e.rawDB(t).QueryRow(`SELECT token_hash FROM push_tokens WHERE label = 'laptop'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hash {
		t.Fatal("the stored hash isn't the SHA-256 this test looks for, so the test would show nothing")
	}
	for _, p := range []string{"/account", "/users", "/"} {
		page := body(t, e.getAs(t, tok, p))
		if strings.Contains(page, raw) {
			t.Errorf("%s shows the token", p)
		}
		if strings.Contains(page, hash) {
			t.Errorf("%s shows the token's hash", p)
		}
	}
}
