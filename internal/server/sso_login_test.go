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
// SSO connected, and discovery returning p.
func ssoLoginEnv(t *testing.T, p sso.Provider) *testEnv {
	t.Helper()
	e := newTestEnv(t)
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
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeIdentity("sub-1", "dana@example.com")
			e := ssoLoginEnv(t, fake)
			resp := c.run(t, e, fake)
			resp.Body.Close()
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

// rawCallback sends a callback carrying exactly one cookie.
func rawCallback(t *testing.T, e *testEnv, name, value string, query url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/auth/sso/callback?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", name+"="+value)
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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

func TestFlowCookieIsSecureOverHTTPS(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	req, _ := http.NewRequest(http.MethodGet, e.http.URL+"/auth/sso/start", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, ssoStateCookiePrefix) && !c.Secure {
			t.Errorf("flow cookie %s isn't Secure over HTTPS", c.Name)
		}
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
