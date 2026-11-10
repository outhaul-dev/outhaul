package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"html"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sso"
	"github.com/outhaul-dev/outhaul/internal/store"
)

const (
	ssoButton  = "Sign in with SSO"
	testIssuer = "https://idp.example.com"
)

// ssoLoginEnv has a password admin (so /login doesn't send people to /setup),
// SSO connected, and discovery returning p. The public URL is the test
// server's own, as a sign-in only runs on the public URL.
func ssoLoginEnv(t *testing.T, p sso.Provider) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.srv.publicURL = e.http.URL
	e.signInAs(t, "root", core.RoleSystemAdmin)
	if err := e.store.SetSSOSettings(context.Background(), store.SSOSettings{
		Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec",
	}, false); err != nil {
		t.Fatal(err)
	}
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) { return p, nil }
	return e
}

// passwordAdmin adds a system admin who can sign in with password.
func passwordAdmin(t *testing.T, e *testEnv, name, password string) {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateUser(context.Background(), name, hash); err != nil {
		t.Fatal(err)
	}
}

func fakeIdentity(sub, email string) *sso.Fake {
	return &sso.Fake{Identity: sso.Identity{Issuer: testIssuer, Subject: sub, Email: email}}
}

// browser is one person's browser: its own cookie jar, and redirects are not
// followed so each step can be checked.
type browser struct {
	e      *testEnv
	client *http.Client
}

func newBrowser(e *testEnv) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, client: &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := b.client.Get(b.e.http.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (b *browser) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := b.client.PostForm(b.e.http.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (b *browser) cookie(name string) (*http.Cookie, bool) {
	u, _ := url.Parse(b.e.http.URL + "/auth/sso/callback")
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

func (b *browser) flowCookies() int {
	u, _ := url.Parse(b.e.http.URL + "/auth/sso/callback")
	n := 0
	for _, c := range b.client.Jar.Cookies(u) {
		if strings.HasPrefix(c.Name, ssoStateCookiePrefix) {
			n++
		}
	}
	return n
}

// start opens /auth/sso/start and returns the state the provider was sent.
func (b *browser) start(t *testing.T) (state string) {
	t.Helper()
	resp := b.get(t, "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start = %d to %q, want 302 to the provider", resp.StatusCode, resp.Header.Get("Location"))
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host != "sso.fake.invalid" {
		t.Fatalf("start redirected to %q", resp.Header.Get("Location"))
	}
	state = loc.Query().Get("state")
	if len(state) < 32 {
		t.Fatalf("state %q is too short", state)
	}
	return state
}

func (b *browser) callback(t *testing.T, query url.Values) *http.Response {
	t.Helper()
	return b.get(t, "/auth/sso/callback?"+query.Encode())
}

// signIn runs the whole flow and returns the new session token.
func (b *browser) signIn(t *testing.T) string {
	t.Helper()
	state := b.start(t)
	resp := b.callback(t, url.Values{"state": {state}, "code": {"code-" + state}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	c, ok := b.cookie(sessionCookie)
	if !ok {
		t.Fatal("no session cookie after signing in")
	}
	return c.Value
}

func sessionCookieIn(resp *http.Response) (*http.Cookie, bool) {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c, true
		}
	}
	return nil, false
}

func ssoAccounts(t *testing.T, e *testEnv) []core.User {
	t.Helper()
	list, err := e.store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []core.User
	for _, u := range list {
		if u.SignIn == core.SignInSSO {
			out = append(out, u.User)
		}
	}
	return out
}

// flowProvider is a Provider for several sign-ins at once. Each code names the
// state it was issued for, and Exchange accepts it only with the nonce and
// verifier AuthURL was given for that state.
type flowProvider struct {
	mu    sync.Mutex
	flows map[string][2]string // state -> nonce, verifier
	used  int
}

func (p *flowProvider) AuthURL(state, nonce, verifier string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.flows == nil {
		p.flows = map[string][2]string{}
	}
	p.flows[state] = [2]string{nonce, verifier}
	return "https://sso.fake.invalid/authorize?state=" + url.QueryEscape(state)
}

func (p *flowProvider) Exchange(_ context.Context, code, nonce, verifier string) (sso.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := strings.TrimPrefix(code, "code-")
	f, ok := p.flows[state]
	if !ok || f[0] != nonce || f[1] != verifier {
		return sso.Identity{}, errors.New("flow provider: unknown code, or the wrong nonce or verifier")
	}
	delete(p.flows, state) // a code works once
	p.used++
	return sso.Identity{Issuer: testIssuer, Subject: "sub-" + state[:8], Email: state[:8] + "@example.com"}, nil
}

func TestLoginPageShowsSSOButtonOnlyWhenConnected(t *testing.T) {
	e := newTestEnv(t)
	e.signInAs(t, "root", core.RoleSystemAdmin)
	if page := body(t, newBrowser(e).get(t, "/login")); strings.Contains(page, ssoButton) {
		t.Error("button shown with no provider connected")
	}

	e = ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	page := body(t, newBrowser(e).get(t, "/login"))
	button := strings.Index(page, ssoButton)
	form := strings.Index(page, `action="/login"`)
	if button < 0 || form < 0 || button > form {
		t.Fatalf("want the button above the password form: button at %d, form at %d", button, form)
	}
	if !strings.Contains(page, `href="/auth/sso/start"`) {
		t.Error("the button doesn't start a sign-in")
	}

	// Without a public URL the provider can't send anyone back, so no button.
	e.srv.publicURL = ""
	if page := body(t, newBrowser(e).get(t, "/login")); strings.Contains(page, ssoButton) {
		t.Error("button shown with no public URL")
	}
	resp := newBrowser(e).get(t, "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("start with no public URL = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSSOStartWithNothingConnectedGoesToLogin(t *testing.T) {
	e := newTestEnv(t)
	e.srv.publicURL = e.http.URL
	e.signInAs(t, "root", core.RoleSystemAdmin)
	resp := newBrowser(e).get(t, "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("start = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("cookies set: %v", resp.Cookies())
	}
}

func TestSSOFirstSignInCreatesNoRoleAccount(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	b := newBrowser(e)

	resp := b.get(t, "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start = %d, want 302", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	if loc.Host != "sso.fake.invalid" || state == "" || state != fake.LastAuthState {
		t.Fatalf("start redirected to %q; the provider was given state %q", resp.Header.Get("Location"), fake.LastAuthState)
	}
	if fake.LastNonce == "" || fake.LastNonce == state || len(fake.LastVerifier) < 43 {
		t.Fatalf("nonce %q, verifier %q: want a nonce of its own and a PKCE verifier", fake.LastNonce, fake.LastVerifier)
	}
	var flow *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == ssoStateCookiePrefix+state[:12] {
			flow = c
		}
	}
	if flow == nil {
		t.Fatalf("no flow cookie named from the state in %v", resp.Cookies())
	}
	if !flow.HttpOnly || flow.Path != "/auth/sso" || flow.SameSite != http.SameSiteLaxMode || flow.MaxAge != 600 || flow.Secure {
		t.Errorf("flow cookie = %+v; want HttpOnly, Path /auth/sso, Lax, 10 minutes, not Secure over plain HTTP", flow)
	}
	if strings.Contains(resp.Header.Get("Cache-Control"), "no-store") == false {
		t.Error("start response may be cached")
	}

	resp = b.callback(t, url.Values{"state": {state}, "code": {"C"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	if fake.LastExchangeCode != "C" || fake.LastExchangeNonce != fake.LastNonce || fake.LastExchangeVerifier != fake.LastVerifier {
		t.Errorf("Exchange got code %q nonce %q verifier %q; want C and the flow's own nonce and verifier",
			fake.LastExchangeCode, fake.LastExchangeNonce, fake.LastExchangeVerifier)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == flow.Name && c.MaxAge < 0 && c.Path == "/auth/sso" {
			cleared = true
		}
	}
	if !cleared || b.flowCookies() != 0 {
		t.Error("the flow cookie wasn't cleared")
	}
	sc, ok := sessionCookieIn(resp)
	if !ok || !sc.HttpOnly || sc.Path != "/" {
		t.Fatalf("session cookie = %+v", sc)
	}
	if sc.Expires.After(time.Now().Add(ssoSessionTTL+time.Minute)) || sc.Expires.Before(time.Now().Add(ssoSessionTTL-time.Minute)) {
		t.Errorf("session cookie expires %v, want in 8 hours", sc.Expires)
	}

	accounts := ssoAccounts(t, e)
	if len(accounts) != 1 {
		t.Fatalf("SSO accounts = %+v, want 1", accounts)
	}
	u := accounts[0]
	if u.Role != core.RoleNone || u.Email != "dana@example.com" || u.SSOIssuer != testIssuer || u.SSOSubject != "sub-1" || u.LastSignInAt == nil {
		t.Errorf("account = %+v", u)
	}
	sess, err := e.store.GetSession(context.Background(), sc.Value)
	if err != nil || sess.UserID != u.ID {
		t.Fatalf("session = %+v, %v; want one for account %d", sess, err, u.ID)
	}
	if left := time.Until(sess.ExpiresAt); left > ssoSessionTTL || left < ssoSessionTTL-time.Minute {
		t.Errorf("session expires in %v, want 8 hours, not 7 days", left)
	}

	resp = b.get(t, "/")
	page := body(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, noAccessCopy) || !strings.Contains(page, "dana@example.com") {
		t.Errorf("GET / = %d, want the no-access page naming the account: %s", resp.StatusCode, page)
	}
}

func TestPasswordSessionsStillLastSevenDays(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	sess, err := e.store.GetSession(context.Background(), e.sessionToken)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(sess.ExpiresAt); left > sessionTTL || left < sessionTTL-time.Minute {
		t.Errorf("password session expires in %v, want %v", left, sessionTTL)
	}
}

func TestSSOSecondSignInKeepsRoleAndRefreshesEmail(t *testing.T) {
	fake := fakeIdentity("sub-1", "old@example.com")
	e := ssoLoginEnv(t, fake)
	newBrowser(e).signIn(t)
	first := ssoAccounts(t, e)[0]
	if err := e.store.SetUserRole(context.Background(), first.ID, core.RoleProjectMember); err != nil {
		t.Fatal(err)
	}

	fake.Identity.Email = "new@example.com"
	tok := newBrowser(e).signIn(t)
	accounts := ssoAccounts(t, e)
	if len(accounts) != 1 {
		t.Fatalf("SSO accounts = %+v, want still 1", accounts)
	}
	if got := accounts[0]; got.ID != first.ID || got.Role != core.RoleProjectMember || got.Email != "new@example.com" {
		t.Errorf("after the second sign-in: %+v; want id %d, project_member, new@example.com", got, first.ID)
	}
	if got := e.getAs(t, tok, "/").StatusCode; got != http.StatusOK {
		t.Errorf("GET / with a role = %d, want 200", got)
	}
}

func TestSSONeverLinksByEmail(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	pw, err := e.store.CreatePasswordAccount(context.Background(), "dana@example.com", core.RoleSystemAdmin)
	if err != nil {
		t.Fatal(err)
	}
	tok := newBrowser(e).signIn(t)
	sess, err := e.store.GetSession(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID == pw.ID {
		t.Fatal("the SSO sign-in reached the password account with the same email")
	}
	u, err := e.store.GetUser(context.Background(), sess.UserID)
	if err != nil || u.SignIn != core.SignInSSO || u.Role != core.RoleNone {
		t.Errorf("signed in as %+v, %v; want a new SSO account with no role", u, err)
	}
	if got, _ := e.store.GetUser(context.Background(), pw.ID); got.SignIn != core.SignInPassword || got.Role != core.RoleSystemAdmin {
		t.Errorf("password account changed: %+v", got)
	}
}

// A refused callback goes back to /login with a fixed code, signs nobody in,
// creates nobody, and the login page shows the message with the password form.
func TestCallbackRefusals(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	var secrets []string // values that must never reach the log

	cases := []struct {
		name string
		want string // the error code
		run  func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response
	}{
		{"missing cookie", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			state := newBrowser(e).start(t)
			secrets = append(secrets, state)
			return newBrowser(e).callback(t, url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"no state", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			b := newBrowser(e)
			b.start(t)
			return b.callback(t, url.Values{"code": {"C1"}})
		}},
		{"state mismatch", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			b := newBrowser(e)
			state := b.start(t)
			forged := state[:12] + strings.Repeat("0", len(state)-12)
			if forged == state {
				forged = state[:12] + strings.Repeat("1", len(state)-12)
			}
			secrets = append(secrets, state, forged)
			return b.callback(t, url.Values{"state": {forged}, "code": {"C1"}})
		}},
		{"expired flow", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			state := NewToken()
			secrets = append(secrets, state)
			v := encodeSSOFlow(ssoFlow{State: state, Nonce: NewToken(), Verifier: NewToken(), Expires: time.Now().Add(-time.Second)})
			return rawCallback(t, e, ssoStateCookiePrefix+state[:12], v, url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"cookie not base64", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			state := NewToken()
			return rawCallback(t, e, ssoStateCookiePrefix+state[:12], "%%%not-base64%%%", url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"cookie not JSON", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			state := NewToken()
			v := base64.RawURLEncoding.EncodeToString([]byte("not json"))
			return rawCallback(t, e, ssoStateCookiePrefix+state[:12], v, url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"cookie too large", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			state := NewToken()
			v := encodeSSOFlow(ssoFlow{State: state, Nonce: strings.Repeat("n", 4096), Verifier: NewToken(), Expires: time.Now().Add(time.Minute)})
			return rawCallback(t, e, ssoStateCookiePrefix+state[:12], v, url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"provider error", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			b := newBrowser(e)
			state := b.start(t)
			secrets = append(secrets, state)
			return b.callback(t, url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"<b>provider says no</b>"}})
		}},
		{"provider error with a code", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			b := newBrowser(e)
			state := b.start(t)
			secrets = append(secrets, state)
			return b.callback(t, url.Values{"state": {state}, "code": {"C1"}, "error": {"access_denied"}})
		}},
		{"no code", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			b := newBrowser(e)
			state := b.start(t)
			secrets = append(secrets, state)
			return b.callback(t, url.Values{"state": {state}})
		}},
		{"exchange error", "sso_failed", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			fake.ExchangeErr = errors.New("invalid_grant: <b>provider says no</b>")
			b := newBrowser(e)
			state := b.start(t)
			secrets = append(secrets, state)
			return b.callback(t, url.Values{"state": {state}, "code": {"C1"}})
		}},
		{"no email", "sso_no_email", func(t *testing.T, e *testEnv, fake *sso.Fake) *http.Response {
			fake.ExchangeErr = sso.ErrNoEmail
			b := newBrowser(e)
			state := b.start(t)
			secrets = append(secrets, state)
			return b.callback(t, url.Values{"state": {state}, "code": {"C1"}})
		}},
	}
	// The cases whose callback carried the flow's cookie: it is cleared
	// whatever the outcome.
	sentCookie := map[string]bool{
		"state mismatch": true, "expired flow": true, "cookie not base64": true, "cookie not JSON": true,
		"cookie too large": true, "provider error": true, "provider error with a code": true, "no code": true,
		"exchange error": true, "no email": true,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeIdentity("sub-1", "dana@example.com")
			e := ssoLoginEnv(t, fake)
			resp := c.run(t, e, fake)
			resp.Body.Close()
			if sentCookie[c.name] && !flowCookieCleared(resp) {
				t.Errorf("the flow cookie wasn't cleared: %v", resp.Header.Values("Set-Cookie"))
			}
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error="+c.want {
				t.Fatalf("callback = %d to %q, want 303 to /login?error=%s", resp.StatusCode, resp.Header.Get("Location"), c.want)
			}
			if _, ok := sessionCookieIn(resp); ok {
				t.Error("a session cookie was set")
			}
			if accounts := ssoAccounts(t, e); len(accounts) != 0 {
				t.Errorf("accounts created: %+v", accounts)
			}
			var sessions int
			if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id IN (SELECT id FROM users WHERE sign_in = 'sso')`).Scan(&sessions); err != nil || sessions != 0 {
				t.Errorf("SSO sessions = %d, %v", sessions, err)
			}
			if fake.LastExchangeCode != "" && c.name != "exchange error" && c.name != "no email" {
				t.Errorf("the provider was asked to exchange code %q", fake.LastExchangeCode)
			}
			page := body(t, newBrowser(e).get(t, resp.Header.Get("Location")))
			if !strings.Contains(page, html.EscapeString(ssoLoginErrors[c.want])) || !strings.Contains(page, `action="/login"`) {
				t.Errorf("login page lacks the message %q or the password form", ssoLoginErrors[c.want])
			}
			for _, leak := range []string{"provider says no", "access_denied", "invalid_grant"} {
				if strings.Contains(page, leak) {
					t.Errorf("login page shows the provider's text %q", leak)
				}
			}
		})
	}
	for _, s := range append(secrets, "C1") {
		if strings.Contains(logged.String(), s) {
			t.Errorf("the log contains a state or code %q:\n%s", s, logged.String())
		}
	}
}

// rawCallback sends a plain-HTTP callback carrying exactly one cookie.
func rawCallback(t *testing.T, e *testEnv, name, value string, query url.Values) *http.Response {
	t.Helper()
	return rawGet(t, e, "/auth/sso/callback?"+query.Encode(), "", name+"="+value)
}

// rawGet sends a GET with exactly this Cookie header ("" for none). proto, if
// set, is the X-Forwarded-Proto a proxy in front would send.
func rawGet(t *testing.T, e *testEnv, path, proto, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// flowCookieCleared reports whether resp deletes a flow cookie.
func flowCookieCleared(resp *http.Response) bool {
	for _, c := range resp.Cookies() {
		if (strings.HasPrefix(c.Name, ssoStateCookiePrefix) || strings.HasPrefix(c.Name, "__Host-"+ssoStateCookiePrefix)) && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// The flow cookie is gone after the first callback, so the same state and code
// a second time has nothing to check against.
func TestCallbackReplayIsRefused(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	b := newBrowser(e)
	state := b.start(t)
	q := url.Values{"state": {state}, "code": {"C"}}
	resp := b.callback(t, q)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("first callback = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	fake.LastExchangeCode = ""
	resp = b.callback(t, q)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error=sso_failed" {
		t.Fatalf("replayed callback = %d to %q, want 303 to /login?error=sso_failed", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, ok := sessionCookieIn(resp); ok {
		t.Error("the replay set a session cookie")
	}
	if fake.LastExchangeCode != "" {
		t.Error("the replayed code reached the provider")
	}
}

func TestUnknownLoginErrorShowsNothing(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	resp := newBrowser(e).get(t, "/login?error=%3Cscript%3Ealert(1)%3C%2Fscript%3E")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, "alert-error") || strings.Contains(page, "alert(1)") {
		t.Errorf("GET /login with an unknown error = %d: %s", resp.StatusCode, page)
	}
}

func TestTwoTabsCanSignInAtOnce(t *testing.T) {
	p := &flowProvider{}
	e := ssoLoginEnv(t, p)
	b := newBrowser(e)
	s1, s2 := b.start(t), b.start(t)
	if s1[:12] == s2[:12] {
		t.Fatal("two flows share a cookie name")
	}
	if b.flowCookies() != 2 {
		t.Fatalf("flow cookies = %d, want 2", b.flowCookies())
	}
	for _, s := range []string{s1, s2} {
		resp := b.callback(t, url.Values{"state": {s}, "code": {"code-" + s}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Errorf("callback for %s… = %d to %q, want 303 to /", s[:12], resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if p.used != 2 || b.flowCookies() != 0 {
		t.Errorf("exchanges = %d, flow cookies left = %d; want 2 and 0", p.used, b.flowCookies())
	}
}

// The server keeps nothing per sign-in, so strangers starting sign-ins can't
// push out someone else's.
func TestManyStartsDontBreakAnEarlierOne(t *testing.T) {
	p := &flowProvider{}
	e := ssoLoginEnv(t, p)
	b := newBrowser(e)
	state := b.start(t)
	for i := 0; i < 2000; i++ {
		resp := e.getAs(t, "", "/auth/sso/start")
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("start %d = %d", i, resp.StatusCode)
		}
	}
	resp := b.callback(t, url.Values{"state": {state}, "code": {"code-" + state}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("first callback = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestProviderDownAtStart(t *testing.T) {
	e := ssoLoginEnv(t, nil)
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) {
		return nil, errors.New("sso: discovery failed: <b>dial tcp: no route</b>")
	}
	passwordAdmin(t, e, "admin", "supersecret")
	b := newBrowser(e)
	resp := b.get(t, "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error=sso_unavailable" {
		t.Fatalf("start = %d to %q, want 303 to /login?error=sso_unavailable", resp.StatusCode, resp.Header.Get("Location"))
	}
	if b.flowCookies() != 0 {
		t.Error("a flow cookie was set although nothing was started")
	}
	page := body(t, b.get(t, resp.Header.Get("Location")))
	if !strings.Contains(page, html.EscapeString(ssoLoginErrors["sso_unavailable"])) || !strings.Contains(page, `action="/login"`) || strings.Contains(page, "dial tcp") {
		t.Errorf("login page: %s", page)
	}
	resp = b.post(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("password sign-in = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A provider that's gone between start and callback is "unavailable", not a
// failed sign-in.
func TestProviderGoneAtCallback(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	b := newBrowser(e)
	state := b.start(t)
	e.srv.dropSSOProvider()
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) { return nil, errors.New("down") }
	resp := b.callback(t, url.Values{"state": {state}, "code": {"C"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error=sso_unavailable" {
		t.Errorf("callback = %d to %q, want 303 to /login?error=sso_unavailable", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(ssoAccounts(t, e)) != 0 {
		t.Error("an account was created")
	}
}

func TestSignedInPersonStartingSSOGoesHome(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	resp := e.getAs(t, e.signInAs(t, "pat", core.RoleProjectViewer), "/auth/sso/start")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("start while signed in = %d to %q, want 303 to /", resp.StatusCode, resp.Header.Get("Location"))
	}
	if fake.LastAuthState != "" {
		t.Error("a sign-in was started")
	}
}

// Signing in with SSO over an existing session replaces it: the old token stops
// working.
func TestSSOSignInReplacesAnOldSession(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	old := e.signInAs(t, "pat", core.RoleProjectViewer)
	b := newBrowser(e)
	state := b.start(t)
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: old, Path: "/"}})
	resp := b.callback(t, url.Values{"state": {state}, "code": {"C"}})
	resp.Body.Close()
	sc, ok := sessionCookieIn(resp)
	if resp.StatusCode != http.StatusSeeOther || !ok || sc.Value == old {
		t.Fatalf("callback = %d, session cookie %+v; want a new one", resp.StatusCode, sc)
	}
	if hasSession(e, old) {
		t.Error("the old session still works")
	}
}

// publicAt makes the public URL scheme://<the test server's address>, so
// requests to the test server arrive on it whatever they look like.
func publicAt(e *testEnv, scheme string) {
	u, _ := url.Parse(e.http.URL)
	e.srv.publicURL = scheme + "://" + u.Host
}

// The flow cookie follows the public URL's scheme, where the callback always
// arrives, not the request's. In tunnel mode Traefik serves plain HTTP and
// says so in X-Forwarded-Proto while browsers are on HTTPS. Over HTTPS it is a
// __Host- cookie, which a sibling subdomain can't set, and the whole sign-in
// still works.
func TestFlowCookieFollowsThePublicURL(t *testing.T) {
	for _, c := range []struct {
		public, proto string
	}{
		{"https", ""},
		{"https", "http"}, // tunnel mode
		{"https", "https"},
		{"http", ""},
		{"http", "https"},
	} {
		fake := fakeIdentity("sub-1", "dana@example.com")
		e := ssoLoginEnv(t, fake)
		publicAt(e, c.public)
		resp := rawGet(t, e, "/auth/sso/start", c.proto, "")
		resp.Body.Close()
		state := fake.LastAuthState
		if resp.StatusCode != http.StatusFound || state == "" {
			t.Fatalf("%s public URL, proto %q: start = %d, state %q", c.public, c.proto, resp.StatusCode, state)
		}
		var flow *http.Cookie
		for _, ck := range resp.Cookies() {
			if strings.Contains(ck.Name, ssoStateCookiePrefix) {
				flow = ck
			}
		}
		if flow == nil {
			t.Fatalf("%s public URL, proto %q: no flow cookie in %v", c.public, c.proto, resp.Header.Values("Set-Cookie"))
		}
		name, path, secure := ssoStateCookiePrefix+state[:12], "/auth/sso", false
		if c.public == "https" {
			name, path, secure = "__Host-"+name, "/", true
		}
		if flow.Name != name || flow.Path != path || flow.Secure != secure || flow.Domain != "" ||
			!flow.HttpOnly || flow.SameSite != http.SameSiteLaxMode || flow.MaxAge != 600 {
			t.Errorf("%s public URL, proto %q: flow cookie = %+v; want %s, Path %s, Secure %v, no Domain, HttpOnly, Lax, 10 minutes",
				c.public, c.proto, flow, name, path, secure)
		}

		resp = rawGet(t, e, "/auth/sso/callback?"+url.Values{"state": {state}, "code": {"C"}}.Encode(), c.proto, flow.Name+"="+flow.Value)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("%s public URL, proto %q: callback = %d to %q, want 303 to /", c.public, c.proto, resp.StatusCode, resp.Header.Get("Location"))
		}
		cleared := false
		for _, ck := range resp.Cookies() {
			if ck.Name == name && ck.MaxAge < 0 && ck.Path == path && ck.Secure == secure {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("%s public URL, proto %q: the flow cookie wasn't cleared with its own name, path and Secure flag: %v",
				c.public, c.proto, resp.Header.Values("Set-Cookie"))
		}
		// The callback is on the public URL's host, so on an HTTPS public URL the
		// session cookie is Secure whatever X-Forwarded-Proto says.
		wantSecure := c.public == "https" || c.proto == "https"
		if sc, ok := sessionCookieIn(resp); !ok || sc.Secure != wantSecure {
			t.Errorf("%s public URL, proto %q: session cookie = %+v, want Secure %v", c.public, c.proto, sc, wantSecure)
		}
	}
}

// The callback looks only for the cookie name of the public URL's scheme: on
// an HTTPS public URL a plain-named cookie, which a sibling subdomain could
// toss in, is ignored, however the request looks.
func TestFlowCookieMustMatchThePublicScheme(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	state := NewToken()
	v := encodeSSOFlow(ssoFlow{State: state, Nonce: NewToken(), Verifier: NewToken(), Expires: time.Now().Add(time.Minute)})
	q := "/auth/sso/callback?" + url.Values{"state": {state}, "code": {"C"}}.Encode()
	for _, c := range []struct {
		public, proto, cookie string
	}{
		{"https", "", ssoStateCookiePrefix + state[:12] + "=" + v},
		{"https", "http", ssoStateCookiePrefix + state[:12] + "=" + v},
		{"http", "https", "__Host-" + ssoStateCookiePrefix + state[:12] + "=" + v},
	} {
		publicAt(e, c.public)
		resp := rawGet(t, e, q, c.proto, c.cookie)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error=sso_failed" {
			t.Errorf("%s public URL, proto %q, with %.30s… = %d to %q, want 303 to /login?error=sso_failed",
				c.public, c.proto, c.cookie, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if fake.LastExchangeCode != "" || len(ssoAccounts(t, e)) != 0 {
		t.Error("a cookie for the other scheme was used")
	}
}

// startOn opens /auth/sso/start with this Host header ("" keeps the test
// server's own) and query, and an optional Cookie header.
func startOn(t *testing.T, e *testEnv, host, query, cookie string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/auth/sso/start"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// A sign-in started anywhere but the public URL's host moves there before
// anything else: the flow cookie belongs to the host that answers start, and
// the provider sends the browser back to the public URL, so it could never
// finish otherwise.
func TestSSOStartMovesToThePublicURL(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	var discovered atomic.Int32
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) {
		discovered.Add(1)
		return fake, nil
	}
	signedIn := e.signInAs(t, "dana", core.RoleSystemAdmin)
	e.srv.publicURL = "https://outhaul.example.com/"

	for _, c := range []struct{ host, cookie string }{
		{"", ""}, // the test server's own address
		{"outhaul.example.com:8080", ""},
		{"10.0.0.5:8080", ""},
		{"apps.example.com", ""},
		{"outhaul.example.com:8080", sessionCookie + "=" + signedIn},
	} {
		resp := startOn(t, e, c.host, "?a=1&b=two", c.cookie)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://outhaul.example.com/auth/sso/start?a=1&b=two" {
			t.Errorf("start on %q (cookie %v) = %d to %q, want 302 to the same path and query on the public URL",
				c.host, c.cookie != "", resp.StatusCode, resp.Header.Get("Location"))
		}
		if len(resp.Cookies()) != 0 {
			t.Errorf("start on %q set cookies: %v", c.host, resp.Header.Values("Set-Cookie"))
		}
	}
	if discovered.Load() != 0 || fake.LastAuthState != "" {
		t.Fatalf("start did something before moving: %d discoveries, state %q", discovered.Load(), fake.LastAuthState)
	}

	// On the public URL's host, whatever its case, it starts: no loop. A port
	// the scheme implies is the same host.
	for _, c := range []struct{ public, host string }{
		{"https://outhaul.example.com", "outhaul.example.com"},
		{"https://outhaul.example.com", "OUTHAUL.Example.com"},
		{"https://outhaul.example.com:443", "outhaul.example.com"},
		{"https://outhaul.example.com", "outhaul.example.com:443"},
		{"http://outhaul.example.com:8080", "outhaul.example.com:8080"},
		{"http://outhaul.example.com", "outhaul.example.com:80"},
	} {
		e.srv.publicURL = c.public
		resp := startOn(t, e, c.host, "", "")
		if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusFound || loc == nil || loc.Host != "sso.fake.invalid" {
			t.Errorf("public URL %s, start on %q = %d to %q, want 302 to the provider", c.public, c.host, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// A different port is a different host.
	e.srv.publicURL = "http://outhaul.example.com:8080"
	resp := startOn(t, e, "outhaul.example.com", "", "")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://outhaul.example.com:8080/auth/sso/start" {
		t.Errorf("start on another port = %d to %q, want 302 to the public URL", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSSOFlowRoundTrip(t *testing.T) {
	f := ssoFlow{State: NewToken(), Nonce: NewToken(), Verifier: NewToken(), Expires: time.Now().Add(time.Minute).Round(0).UTC()}
	got, ok := decodeSSOFlow(encodeSSOFlow(f))
	if !ok || got.State != f.State || got.Nonce != f.Nonce || got.Verifier != f.Verifier || !got.Expires.Equal(f.Expires) {
		t.Fatalf("round trip = %+v, %v; want %+v", got, ok, f)
	}
	for _, bad := range []string{"", "%%%", base64.RawURLEncoding.EncodeToString([]byte("{}")), strings.Repeat("A", 4097)} {
		if _, ok := decodeSSOFlow(bad); ok {
			t.Errorf("decoded %.20q", bad)
		}
	}
}

func TestAccountPageForSSOPeople(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	member := ssoAccount(t, e, "mina@example.com", core.RoleProjectMember)

	resp := e.getAs(t, member, "/account")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Push keys") || strings.Contains(page, "Change password") {
		t.Errorf("GET /account = %d; want push keys and no Change password panel: %s", resp.StatusCode, page)
	}
	resp = e.postAs(t, member, "/account/password", url.Values{"current": {""}, "new": {"supersecret"}})
	page = body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "SSO") || strings.Contains(page, "store:") {
		t.Errorf("POST /account/password = %d %q, want 400 with a plain message", resp.StatusCode, page)
	}
	var hash string
	if err := e.rawDB(t).QueryRow(`SELECT password_hash FROM users WHERE email = 'mina@example.com'`).Scan(&hash); err != nil || hash != "" {
		t.Errorf("password_hash = %q, %v; want none", hash, err)
	}

	none := ssoAccount(t, e, "nora@example.com", core.RoleNone)
	resp = e.getAs(t, none, "/account")
	page = body(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, noAccessCopy) {
		t.Errorf("GET /account with no role = %d, want the no-access page: %s", resp.StatusCode, page)
	}
	resp = e.postAs(t, none, "/account/push-keys", url.Values{"label": {"k"}, "public_key": {testKeyA}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("adding a push key with no role = %d, want 403", resp.StatusCode)
	}
	var keys int
	if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM push_keys`).Scan(&keys); err != nil || keys != 0 {
		t.Errorf("push keys = %d, %v; want 0", keys, err)
	}
	// A password account with no role still has its account page.
	pw := e.signInAs(t, "pat", core.RoleNone)
	if got := e.getAs(t, pw, "/account").StatusCode; got != http.StatusOK {
		t.Errorf("GET /account for a password account with no role = %d, want 200", got)
	}
}

func TestPasswordFormRefusesSSOAccounts(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	newBrowser(e).signIn(t)
	u := ssoAccounts(t, e)[0]
	// Even with a password hash planted on the row, the form never finds it.
	hash, err := HashPassword("supersecret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.rawDB(t).Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, u.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{u.Email, u.Username} {
		resp := newBrowser(e).post(t, "/login", url.Values{"username": {name}, "password": {"supersecret"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST /login as %q = %d, want 401", name, resp.StatusCode)
		}
		if _, ok := sessionCookieIn(resp); ok {
			t.Errorf("POST /login as %q set a session", name)
		}
	}
}

func TestUsersPageExplainsRemovingSSOPerson(t *testing.T) {
	e := newTestEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	e.signInAs(t, "pat", core.RoleProjectViewer)
	insertSSOUser(t, e, "dana@example.com", core.RoleProjectMember)
	page := body(t, e.getAs(t, admin, "/users"))
	row := func(name string) string {
		for _, r := range strings.Split(page, "<tr")[1:] {
			if strings.Contains(r, name) {
				return r
			}
		}
		t.Fatalf("no row for %s", name)
		return ""
	}
	sso, pw := row("dana@example.com"), row("pat")
	if !strings.Contains(sso, "SSO") {
		t.Error("the SSO row doesn't say how they sign in")
	}
	for _, want := range []string{ssoRemoveNote, ssoKeysNote} {
		if !strings.Contains(sso, want) {
			t.Errorf("the SSO row lacks %q", want)
		}
		if strings.Contains(pw, want) {
			t.Errorf("the password row has %q", want)
		}
	}
}

// What the Users page says on an SSO row, in its rendered form.
const (
	ssoRemoveNote = "Also remove them at your SSO provider, or they could sign in again and come back with no role."
	ssoKeysNote   = "Their push keys stay until you remove them here."
)

// hookProvider runs during() while Exchange is talking to the provider, standing
// in for a settings change that lands mid-sign-in.
type hookProvider struct {
	*sso.Fake
	during func()
}

func (h *hookProvider) Exchange(ctx context.Context, code, nonce, verifier string) (sso.Identity, error) {
	if h.during != nil {
		h.during()
	}
	return h.Fake.Exchange(ctx, code, nonce, verifier)
}

// A change that ends SSO sessions also ends a sign-in still in flight.
func TestSignInInFlightDuringASettingsChangeIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		change func(e *testEnv) error
		ok     bool
	}{
		{"removed", func(e *testEnv) error { return e.store.ClearSSOSettings(ctx) }, false},
		{"issuer changed", func(e *testEnv) error {
			return e.store.SetSSOSettings(ctx, store.SSOSettings{Issuer: "https://other.example.com", ClientID: "cid", ClientSecret: "sec"}, true)
		}, false},
		{"client ID changed", func(e *testEnv) error {
			return e.store.SetSSOSettings(ctx, store.SSOSettings{Issuer: testIssuer, ClientID: "cid2", ClientSecret: "sec"}, true)
		}, false},
		{"only the secret changed", func(e *testEnv) error {
			return e.store.SetSSOSettings(ctx, store.SSOSettings{Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec2"}, false)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := &hookProvider{Fake: fakeIdentity("sub-1", "dana@example.com")}
			e := ssoLoginEnv(t, p)
			p.during = func() {
				if err := c.change(e); err != nil {
					t.Error(err)
				}
			}
			b := newBrowser(e)
			state := b.start(t)
			resp := b.callback(t, url.Values{"state": {state}, "code": {"C"}})
			resp.Body.Close()
			_, cookie := sessionCookieIn(resp)
			if c.ok {
				if resp.Header.Get("Location") != "/" || !cookie {
					t.Errorf("callback = %d to %q, want a sign-in", resp.StatusCode, resp.Header.Get("Location"))
				}
				return
			}
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?error=sso_failed" || cookie {
				t.Errorf("callback = %d to %q (session cookie %v), want 303 to /login?error=sso_failed and none", resp.StatusCode, resp.Header.Get("Location"), cookie)
			}
			if accounts := ssoAccounts(t, e); len(accounts) != 0 {
				t.Errorf("accounts created: %+v", accounts)
			}
		})
	}
}

// A change that commits just as the session is written is caught too: the
// settings are checked again after the session exists, so either that check
// sees the change or the change's own transaction ends the session.
func TestSettingsChangeAsTheSessionIsWrittenEndsIt(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	if _, err := e.rawDB(t).Exec(`CREATE TRIGGER change_sso AFTER INSERT ON sessions
		BEGIN UPDATE settings SET value = 'cid2' WHERE key = 'sso_client_id'; END`); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(e)
	state := b.start(t)
	resp := b.callback(t, url.Values{"state": {state}, "code": {"C"}})
	resp.Body.Close()
	if _, ok := sessionCookieIn(resp); ok || resp.Header.Get("Location") != "/login?error=sso_failed" {
		t.Errorf("callback = %d to %q, cookie %v; want refused with no session cookie", resp.StatusCode, resp.Header.Get("Location"), ok)
	}
	var sessions int
	if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id IN (SELECT id FROM users WHERE sign_in = 'sso')`).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("SSO sessions = %d, %v; want 0", sessions, err)
	}
}

// An SSO account's username is internal: every page shows its email.
func TestSSOUsernameIsNeverShown(t *testing.T) {
	ctx := context.Background()
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	tok := newBrowser(e).signIn(t)
	u := ssoAccounts(t, e)[0]
	admin := e.signInAs(t, "boss", core.RoleSystemAdmin)

	check := func(what, token, path string) {
		t.Helper()
		page := body(t, e.getAs(t, token, path))
		if strings.Contains(page, u.Username) || strings.Contains(page, "sso:") {
			t.Errorf("%s shows the SSO username", what)
		}
		if !strings.Contains(page, "dana@example.com") {
			t.Errorf("%s doesn't show the email", what)
		}
	}
	check("the no-access page", tok, "/")
	if err := e.store.SetUserRole(ctx, u.ID, core.RoleProjectMember); err != nil {
		t.Fatal(err)
	}
	on := seedProject(t, e, "alpha")
	off := seedProject(t, e, "bravo")
	if err := e.store.AddProjectMember(ctx, on.Project.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	check("the Users page", admin, "/users")
	check("the People panel", admin, "/projects/"+itoa(on.Project.ID))
	check("the list of people to add", admin, "/projects/"+itoa(off.Project.ID))
	check("the account page", tok, "/account")
}

// The first-boot form refuses a username that starts with "sso:", in any case,
// with the Users page's message.
func TestSetupRefusesSSOUsernames(t *testing.T) {
	e := newTestEnv(t)
	for _, name := range []string{"sso:root", "SSO:root", "Sso:" + strings.Repeat("a", 64)} {
		resp := e.postForm(t, "/setup", url.Values{"token": {"SETUPTOKEN"}, "username": {name}, "password": {"supersecret"}})
		page := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, html.EscapeString(ssoPrefixRefusal)) {
			t.Errorf("setup as %q = %d, want 400 with %q", name, resp.StatusCode, ssoPrefixRefusal)
		}
	}
	if has, _ := e.store.HasUser(context.Background()); has {
		t.Fatal("an account was created")
	}
	// The Users page says the same.
	if err := validUsername("SSO:x"); err == nil || err.Error() != ssoPrefixRefusal {
		t.Errorf("validUsername = %v, want %q", err, ssoPrefixRefusal)
	}
	e.completeSetup(t)
}

// While discovery fails, a flood of sign-ins makes one call to the provider
// per ssoRetryAfter, not one each. Saving the settings always runs a fresh
// discovery, and saving or removing them forgets the failure.
func TestDiscoveryFailureIsRememberedBriefly(t *testing.T) {
	e := ssoLoginEnv(t, nil)
	admin := e.signInAs(t, "boss", core.RoleSystemAdmin)
	var calls atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	e.srv.discover = func(context.Context, sso.Config) (sso.Provider, error) {
		calls.Add(1)
		if failing.Load() {
			time.Sleep(100 * time.Millisecond) // a slow provider: the flood arrives meanwhile
			return nil, errors.New("sso: discovery failed: no route to host")
		}
		return fakeIdentity("sub-1", "dana@example.com"), nil
	}
	start := func() *http.Response {
		resp := e.getAs(t, "", "/auth/sso/start")
		resp.Body.Close()
		return resp
	}
	unavailable := func(resp *http.Response) bool {
		return resp.StatusCode == http.StatusSeeOther && resp.Header.Get("Location") == "/login?error=sso_unavailable"
	}

	var wg sync.WaitGroup
	results := make([]*http.Response, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = start()
		}()
	}
	wg.Wait()
	for i, resp := range results {
		if !unavailable(resp) {
			t.Fatalf("start %d = %d to %q, want sso_unavailable", i, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for i := 0; i < 10; i++ {
		start()
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("discovery ran %d times for 60 sign-ins, want 1", n)
	}

	// Saving runs its own discovery every time, even while a failure is
	// remembered.
	form := url.Values{"issuer": {testIssuer}, "client_id": {"cid"}, "client_secret": {""}}
	for i := 0; i < 2; i++ {
		resp := e.postAs(t, admin, "/settings/sso", form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("save with discovery failing = %d, want 400", resp.StatusCode)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("discovery ran %d times after two saves, want 3", n)
	}

	// A successful save forgets the failure: the next sign-in discovers again.
	failing.Store(false)
	if resp := e.postAs(t, admin, "/settings/sso", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d, want 303", resp.StatusCode)
	}
	if resp := start(); resp.StatusCode != http.StatusFound {
		t.Fatalf("start after a save = %d to %q, want 302 to the provider", resp.StatusCode, resp.Header.Get("Location"))
	}

	// So does removing the settings.
	failing.Store(true)
	e.srv.dropSSOProvider()
	if resp := start(); !unavailable(resp) {
		t.Fatalf("start = %d, want sso_unavailable", resp.StatusCode)
	}
	if resp := e.postAs(t, admin, "/settings/sso/delete", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove = %d, want 303", resp.StatusCode)
	}
	if err := e.store.SetSSOSettings(context.Background(), store.SSOSettings{Issuer: testIssuer, ClientID: "cid", ClientSecret: "sec"}, false); err != nil {
		t.Fatal(err)
	}
	failing.Store(false)
	if resp := start(); resp.StatusCode != http.StatusFound {
		t.Fatalf("start after removing and reconnecting = %d to %q, want 302", resp.StatusCode, resp.Header.Get("Location"))
	}

	// And the failure is only remembered for ssoRetryAfter.
	e.srv.ssoRetryAfter = 20 * time.Millisecond
	failing.Store(true)
	e.srv.dropSSOProvider()
	before := calls.Load()
	start()
	time.Sleep(50 * time.Millisecond)
	start()
	if n := calls.Load() - before; n != 2 {
		t.Errorf("discovery ran %d times across an expired failure, want 2", n)
	}
}
