package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sso"
	"github.com/outhaul-dev/outhaul/internal/store"
)

// ssoEnv is a signed-in system admin whose discovery succeeds with a Fake and
// records the config it was given.
func ssoEnv(t *testing.T) (*testEnv, *[]sso.Config) {
	t.Helper()
	e := newTestEnv(t)
	e.completeSetup(t)
	var seen []sso.Config
	e.srv.discover = func(ctx context.Context, cfg sso.Config) (sso.Provider, error) {
		seen = append(seen, cfg)
		return &sso.Fake{}, nil
	}
	return e, &seen
}

func saveSSO(t *testing.T, e *testEnv, issuer, clientID, secret string, extra ...string) *http.Response {
	t.Helper()
	f := url.Values{"issuer": {issuer}, "client_id": {clientID}, "client_secret": {secret}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return e.postForm(t, "/settings/sso", f)
}

// ssoSession makes an SSO account with a live session and returns the token.
func ssoSession(t *testing.T, e *testEnv) string {
	t.Helper()
	res, err := e.rawDB(t).Exec(`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		VALUES ('sso:s1', '', ?, 'system_admin', 'sso', 'https://idp.example.com', 's1', 's1@example.com')`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	tok := NewToken()
	if err := e.store.CreateSession(context.Background(), core.Session{Token: tok, UserID: id, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return tok
}

func hasSession(e *testEnv, tok string) bool {
	_, err := e.store.GetSession(context.Background(), tok)
	return err == nil
}

func TestSSOPanelShowsRedirectURIAndWarning(t *testing.T) {
	e, _ := ssoEnv(t)
	page := body(t, e.get(t, "/settings"))
	for _, want := range []string{
		"Single sign-on (SSO)",
		"https://slip.example.com/auth/sso/callback",
		"Everyone the provider lets sign in can reach every protected route",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
}

func TestSSOPanelNeedsPublicURL(t *testing.T) {
	e, _ := ssoEnv(t)
	e.srv.publicURL = ""
	page := body(t, e.get(t, "/settings"))
	if !strings.Contains(page, "Single sign-on needs <code>OUTHAUL_PUBLIC_URL</code>") {
		t.Error("panel does not say SSO needs OUTHAUL_PUBLIC_URL")
	}
	resp := saveSSO(t, e, "https://idp.example.com", "cid", "sec")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("save status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if strings.Contains(page, "Remove SSO") {
		t.Error("offers to remove SSO when none is connected")
	}
}

// Settings saved before OUTHAUL_PUBLIC_URL was unset can still be removed.
func TestSSOCanBeRemovedWithoutPublicURL(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	tok := ssoSession(t, e)
	e.srv.publicURL = ""
	if page := body(t, e.get(t, "/settings")); !strings.Contains(page, "Remove SSO") {
		t.Error("no way to remove the stored settings")
	}
	resp := e.postForm(t, "/settings/sso/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", resp.StatusCode)
	}
	if _, ok, _ := e.store.GetSSOSettings(context.Background()); ok {
		t.Error("settings remain")
	}
	if hasSession(e, tok) {
		t.Error("SSO session remains")
	}
}

func TestSaveRunsDiscovery(t *testing.T) {
	e, seen := ssoEnv(t)
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) {
		return nil, errors.New("sso: discovery failed: no such host")
	}
	resp := saveSSO(t, e, "https://idp.example.com", "cid", "topsecret")
	page := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(page, "discovery failed: no such host") || strings.Contains(page, "sso: discovery") {
		t.Errorf("page should show the reason without the sso: prefix")
	}
	if strings.Contains(page, "topsecret") {
		t.Error("secret rendered")
	}
	if !strings.Contains(page, "https://idp.example.com") {
		t.Error("form lost what the admin typed")
	}
	if _, ok, _ := e.store.GetSSOSettings(context.Background()); ok {
		t.Fatal("settings stored despite failed discovery")
	}

	e.srv.discover = func(_ context.Context, cfg sso.Config) (sso.Provider, error) {
		*seen = append(*seen, cfg)
		return &sso.Fake{}, nil
	}
	resp = saveSSO(t, e, " https://idp.example.com ", " cid ", "topsecret")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	got, ok, _ := e.store.GetSSOSettings(context.Background())
	if !ok || got.Issuer != "https://idp.example.com" || got.ClientID != "cid" || got.ClientSecret != "topsecret" {
		t.Fatalf("stored %+v ok=%v", got, ok)
	}
	if len(*seen) != 1 || (*seen)[0].RedirectURL != "https://slip.example.com/auth/sso/callback" {
		t.Errorf("discover config = %+v", *seen)
	}
	page = body(t, e.get(t, "/settings"))
	if strings.Contains(page, "topsecret") {
		t.Error("secret rendered after save")
	}
	if !strings.Contains(page, "A client secret is saved. Leave this blank to keep it.") {
		t.Error("panel does not say a secret is saved")
	}
}

func TestBlankSecretKeepsStoredOne(t *testing.T) {
	e, seen := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "first").Body.Close()
	tok := ssoSession(t, e)
	resp := saveSSO(t, e, "https://idp.example.com", "cid", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _, _ := e.store.GetSSOSettings(context.Background())
	if got.ClientSecret != "first" {
		t.Errorf("secret = %q, want first", got.ClientSecret)
	}
	if last := (*seen)[len(*seen)-1]; last.ClientSecret != "first" {
		t.Errorf("discovery got secret %q", last.ClientSecret)
	}
	if !hasSession(e, tok) {
		t.Error("SSO session ended")
	}
	// Replacing only the secret keeps sessions too.
	saveSSO(t, e, "https://idp.example.com", "cid", "second").Body.Close()
	if got, _, _ := e.store.GetSSOSettings(context.Background()); got.ClientSecret != "second" {
		t.Errorf("secret = %q, want second", got.ClientSecret)
	}
	if !hasSession(e, tok) {
		t.Error("SSO session ended by a secret-only change")
	}
}

func TestFirstSaveEndsNoSessions(t *testing.T) {
	e, _ := ssoEnv(t)
	tok := ssoSession(t, e)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	if !hasSession(e, tok) {
		t.Error("first save ended an SSO session")
	}
}

func TestIssuerChangeNeedsConfirmation(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	tok := ssoSession(t, e)

	resp := saveSSO(t, e, "https://other.example.com", "cid", "")
	page := body(t, resp)
	if !strings.Contains(page, "everyone will come back as a new account with no role") {
		t.Errorf("no confirmation text (status %d)", resp.StatusCode)
	}
	if !strings.Contains(page, "confirm_issuer_change") || !strings.Contains(page, "https://other.example.com") {
		t.Error("re-rendered form lacks the checkbox or the typed issuer")
	}
	got, _, _ := e.store.GetSSOSettings(context.Background())
	if got.Issuer != "https://idp.example.com" {
		t.Fatalf("issuer changed without confirmation: %q", got.Issuer)
	}
	if !hasSession(e, tok) {
		t.Fatal("session ended without confirmation")
	}

	resp = saveSSO(t, e, "https://other.example.com", "cid", "", "confirm_issuer_change", "1")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed status = %d", resp.StatusCode)
	}
	got, _, _ = e.store.GetSSOSettings(context.Background())
	if got.Issuer != "https://other.example.com" || got.ClientSecret != "sec" {
		t.Errorf("stored %+v", got)
	}
	if hasSession(e, tok) {
		t.Error("SSO session survived an issuer change")
	}
}

func TestClientIDChangeEndsSSOSessions(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	tok := ssoSession(t, e)
	resp := saveSSO(t, e, "https://idp.example.com", "cid2", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if hasSession(e, tok) {
		t.Error("SSO session survived a client ID change")
	}
}

func TestRemovingSSOKeepsPasswordsWorking(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	tok := ssoSession(t, e)
	pw := e.signInAs(t, "pat", "system_admin")

	resp := e.postForm(t, "/settings/sso/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, ok, _ := e.store.GetSSOSettings(context.Background()); ok {
		t.Error("settings remain")
	}
	if hasSession(e, tok) {
		t.Error("SSO session remains")
	}
	if !hasSession(e, pw) {
		t.Error("password session ended")
	}
	if _, err := e.srv.ssoProvider(context.Background()); !errors.Is(err, errSSONotConfigured) {
		t.Errorf("ssoProvider err = %v, want errSSONotConfigured", err)
	}
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := anon.Get(e.http.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body(t, r), "Sign in with SSO") {
		t.Error("login page offers SSO")
	}
	resp = e.postForm(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("password login status = %d", resp.StatusCode)
	}
}

func TestSSOProviderIsCachedUntilSettingsChange(t *testing.T) {
	e, seen := ssoEnv(t)
	ctx := context.Background()
	if _, err := e.srv.ssoProvider(ctx); !errors.Is(err, errSSONotConfigured) {
		t.Fatalf("err = %v, want errSSONotConfigured", err)
	}
	if err := e.store.SetSSOSettings(ctx, store.SSOSettings{Issuer: "https://idp.example.com", ClientID: "c", ClientSecret: "s"}, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.srv.ssoProvider(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("discovered %d times, want 1", len(*seen))
	}
	saveSSO(t, e, "https://idp.example.com", "c", "s2").Body.Close()
	n := len(*seen)
	if _, err := e.srv.ssoProvider(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != n+1 {
		t.Errorf("cache not dropped on save")
	}
	// A failed discovery is remembered briefly, until the settings change.
	e.srv.dropSSOProvider()
	fails := 0
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) {
		fails++
		return nil, errors.New("down")
	}
	e.srv.ssoProvider(ctx)
	if _, err := e.srv.ssoProvider(ctx); err == nil || err.Error() != "down" {
		t.Errorf("remembered failure = %v, want the discovery's error", err)
	}
	if fails != 1 {
		t.Errorf("discovery ran %d times while failing, want 1", fails)
	}
	e.srv.dropSSOProvider()
	e.srv.ssoProvider(ctx)
	if fails != 2 {
		t.Errorf("a settings change didn't forget the failure: %d attempts", fails)
	}
}

// A save that can't end the old sessions changes nothing, so the retry still
// sees a changed client ID and ends them.
func TestFailedSaveKeepsOldSettingsSoARetryEndsSessions(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://idp.example.com", "cid", "sec").Body.Close()
	tok := ssoSession(t, e)
	db := e.rawDB(t)
	if _, err := db.Exec(`CREATE TRIGGER fail_session_delete BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'sessions are read-only'); END`); err != nil {
		t.Fatal(err)
	}
	resp := saveSSO(t, e, "https://idp.example.com", "cid2", "")
	page := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(page, "read-only") {
		t.Fatalf("status = %d %q, want 500 without the store's error", resp.StatusCode, page)
	}
	if got, _, _ := e.store.GetSSOSettings(context.Background()); got.ClientID != "cid" {
		t.Fatalf("client ID = %q after a failed save, want cid", got.ClientID)
	}
	resp = e.postForm(t, "/settings/sso/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500", resp.StatusCode)
	}
	if _, ok, _ := e.store.GetSSOSettings(context.Background()); !ok {
		t.Fatal("settings removed although the sessions weren't ended")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_session_delete`); err != nil {
		t.Fatal(err)
	}
	saveSSO(t, e, "https://idp.example.com", "cid2", "").Body.Close()
	if hasSession(e, tok) {
		t.Error("the retried save left the SSO session alive")
	}
}

// A store failure while saving is logged, and the page says only "internal
// error", not the store's own text.
func TestSaveSSOHidesStoreErrors(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	e, _ := ssoEnv(t)
	if resp := saveSSO(t, e, "https://idp.example.com", "cid", "sec"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first save = %d", resp.StatusCode)
	}
	// A secret that no longer decrypts makes reading the settings fail.
	if _, err := e.rawDB(t).Exec(`UPDATE settings SET value = 'not-sealed' WHERE key = 'sso_client_secret'`); err != nil {
		t.Fatal(err)
	}
	resp := saveSSO(t, e, "https://idp.example.com", "cid", "")
	page := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(page) != "internal error" {
		t.Errorf("save with unreadable settings = %d %q, want 500 internal error", resp.StatusCode, page)
	}
	if !strings.Contains(logged.String(), "decrypt SSO client secret") {
		t.Errorf("the store's error wasn't logged:\n%s", logged.String())
	}
}

func TestSSOFromTheEnvironmentWins(t *testing.T) {
	e, seen := ssoEnv(t)
	saveSSO(t, e, "https://stored.example.com", "stored", "sec").Body.Close()
	e.srv.ssoEnv = SSOEnvironment{
		FromEnvironment: true,
		Issuer:          "https://env.example.com",
		ClientID:        "env-id",
		ClientSecret:    "env-secret",
	}

	page := body(t, e.get(t, "/settings"))
	for _, want := range []string{
		"https://env.example.com",
		"env-id",
		"The client secret was read from a file when Outhaul started.",
		"kept, but not used",
		"https://slip.example.com/auth/sso/callback",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page lacks %q", want)
		}
	}
	if strings.Contains(page, `action="/settings/sso"`) {
		t.Error("the environment's settings still get an editable form")
	}

	*seen = nil // the save above ran its own discovery
	p, err := e.srv.ssoProvider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.issuer != "https://env.example.com" || p.clientID != "env-id" || len(*seen) != 1 ||
		(*seen)[0].Issuer != "https://env.example.com" || (*seen)[0].ClientSecret != "env-secret" {
		t.Fatalf("discovery saw %+v, want the environment's settings", *seen)
	}
}

func TestSaveAndDeleteRefusedWhenTheEnvironmentSetsSSO(t *testing.T) {
	e, _ := ssoEnv(t)
	saveSSO(t, e, "https://stored.example.com", "stored", "sec").Body.Close()
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: "https://env.example.com", ClientID: "env-id", ClientSecret: "env-secret"}

	resp := saveSSO(t, e, "https://other.example.com", "other", "sec")
	body := body(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "OUTHAUL_SSO_ISSUER") {
		t.Errorf("save = %d %s, want 409 naming the variables", resp.StatusCode, body)
	}
	resp = e.postForm(t, "/settings/sso/delete", url.Values{})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
	if got, ok, _ := e.store.GetSSOSettings(context.Background()); !ok || got.Issuer != "https://stored.example.com" {
		t.Errorf("the stored settings changed: %+v, %v", got, ok)
	}
}

func TestSignInWorksWithTheEnvironmentAlone(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	e.srv.publicURL = e.http.URL
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec"}
	fake := fakeIdentity("sub-1", "dana@example.com")
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) { return fake, nil }

	b := newBrowser(e)
	b.signIn(t)
	accounts := ssoAccounts(t, e)
	if len(accounts) != 1 || accounts[0].Email != "dana@example.com" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestProbeSSOUsesTheEnvironmentSettings(t *testing.T) {
	e, seen := ssoEnv(t)
	e.srv.ssoEnv = SSOEnvironment{FromEnvironment: true, Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec",
		GroupsClaim: "groups", Scopes: []string{"groups"}}
	e.srv.ProbeSSO(context.Background())
	if len(*seen) != 1 || (*seen)[0].GroupsClaim != "groups" || len((*seen)[0].Scopes) != 1 {
		t.Fatalf("discovery saw %+v", *seen)
	}
}
