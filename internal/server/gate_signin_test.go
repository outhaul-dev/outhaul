package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/sso"
)

// The gateway cookie's names on the wire, spelled out as the gate tests do.
const (
	wireGateCookie      = "outhaul_gate"        // on a plain-HTTP public URL
	wireGateCookieHTTPS = "__Host-outhaul_gate" // on an https:// public URL
	notProtectedCopy    = "This address isn't protected by Outhaul"
	oddPathCopy         = "Outhaul can't sign you in to that exact address. Open the app's main page and try again."
)

// testNonce is n for the browser holding testPending.
var testNonce = nonceOf(testPending)

// gateRows are the domain rows the sign-in tests use:
//
//	wiki.example.com            protected
//	wiki.example.com/webhooks   open, on another app
//	shop.example.com            open
//	shop.example.com/admin      protected
func seedGateRows(t *testing.T, e *testEnv) {
	t.Helper()
	ctx := context.Background()
	app := func(name string) core.App {
		t.Helper()
		a, err := e.store.CreateApp(ctx, core.App{Name: name, ProjectID: core.DefaultProjectID, RepoURL: "https://example.com/" + name + ".git"})
		if err != nil {
			t.Fatalf("CreateApp(%s): %v", name, err)
		}
		return a
	}
	wiki, hooks, shop := app("wiki"), app("hooks"), app("shop")
	for _, d := range []core.Domain{
		{AppID: wiki.ID, Host: gateWikiHost, Port: 8080, TLS: true, Protected: true},
		{AppID: hooks.ID, Host: gateWikiHost, Path: "/webhooks", Port: 8080, TLS: true},
		{AppID: shop.ID, Host: "shop.example.com", Port: 8080, TLS: true},
		{AppID: shop.ID, Host: "shop.example.com", Path: "/admin", Port: 8080, TLS: true, Protected: true},
	} {
		if _, err := e.store.AddDomain(ctx, d); err != nil {
			t.Fatalf("AddDomain(%s%s): %v", d.Host, d.Path, err)
		}
	}
}

// gateSignInEnv has the public URL on the test server over plain HTTP, so
// every request arrives on the public URL's host and a cookie jar keeps the
// gateway cookie, and the rows of seedGateRows.
func gateSignInEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	publicAt(e, "http")
	seedGateRows(t, e)
	return e
}

// passwordAccount adds a password account with role whose password works.
func passwordAccount(t *testing.T, e *testEnv, name string, role core.Role, password string) core.User {
	t.Helper()
	ctx := context.Background()
	u, err := e.store.CreatePasswordAccount(ctx, name, role)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpdateUserPassword(ctx, u.ID, hash, ""); err != nil {
		t.Fatal(err)
	}
	return u
}

// startPath is the gate's sign-in page for rd and n.
func startPath(rd, n string) string {
	return "/gate/start?" + url.Values{"rd": {rd}, "n": {n}}.Encode()
}

// signInForm is the gate's password form, filled in.
func signInForm(username, password, rd, n string) url.Values {
	return url.Values{"username": {username}, "password": {password}, "rd": {rd}, "n": {n}}
}

// gateRequest sends a request carrying only this gateway cookie ("" for none).
func (e *testEnv) gateRequest(t *testing.T, method, path, gateTok string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if gateTok != "" {
		req.AddCookie(&http.Cookie{Name: e.srv.gateCookieName(), Value: gateTok})
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// count is the number of rows in table.
func count(t *testing.T, e *testEnv, table string) int {
	t.Helper()
	var n int
	if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantCodeRedirect checks resp is a 303 to exactly base with a new code as
// its last query parameter, and returns the code.
func wantCodeRedirect(t *testing.T, what string, resp *http.Response, base string) string {
	t.Helper()
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	prefix := base + sep + wireCodeParam + "="
	loc := resp.Header.Get("Location")
	code := strings.TrimPrefix(loc, prefix)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, prefix) || !isTokenHex(code) {
		t.Errorf("%s: %d to %q, want 303 to %s<a code>", what, resp.StatusCode, loc, prefix)
		return ""
	}
	return code
}

// wantRefusal checks resp is a plain refusal with status and copy: no
// redirect, and no cookie.
func wantRefusal(t *testing.T, what string, resp *http.Response, status int, copy string) {
	t.Helper()
	b := body(t, resp)
	if resp.StatusCode != status || !strings.Contains(b, copy) {
		t.Errorf("%s: %d %q, want %d saying %q", what, resp.StatusCode, b, status, copy)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("%s: a refusal redirects to %q", what, loc)
	}
	wantNoCookie(t, what, resp)
}

// wantPrivate checks an answer is neither cached nor named in a Referer.
func wantPrivate(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if cc, rp := resp.Header.Get("Cache-Control"), resp.Header.Get("Referrer-Policy"); cc != "no-store" || rp != "no-referrer" {
		t.Errorf("%s: Cache-Control %q, Referrer-Policy %q; want no-store and no-referrer", what, cc, rp)
	}
}

// wantPrivatePage checks a page a person reads is not cached, and names its
// URL in a Referer only to Outhaul's own origin. Under no-referrer a browser
// that sends no Sec-Fetch-Site would post the page's forms with Origin: null,
// which cross-origin protection refuses.
func wantPrivatePage(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if cc, rp := resp.Header.Get("Cache-Control"), resp.Header.Get("Referrer-Policy"); cc != "no-store" || rp != "same-origin" {
		t.Errorf("%s: Cache-Control %q, Referrer-Policy %q; want no-store and same-origin", what, cc, rp)
	}
}

// attrAfter is the HTML-unescaped attribute text in page that follows the
// first occurrence of prefix, up to the closing quote; "" if prefix isn't
// there.
func attrAfter(page, prefix string) string {
	_, rest, ok := strings.Cut(page, prefix)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, `"`)
	return html.UnescapeString(v)
}

// wantSentToSignIn checks resp sends a browser to the gate's sign-in page,
// to come back to returnURL, as nobody.
func wantSentToSignIn(t *testing.T, what string, resp *http.Response, returnURL string) {
	t.Helper()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || loc.Path != "/gate/start" || loc.Query().Get("rd") != returnURL || !isTokenHex(loc.Query().Get("n")) {
		t.Errorf("%s: %d to %q, want 302 to sign in for %s", what, resp.StatusCode, resp.Header.Get("Location"), returnURL)
	}
	if setCookie(resp, wireRouteCookie) != nil || resp.Header.Get(wireUserHeader) != "" {
		t.Errorf("%s: passed as someone", what)
	}
}

// redeem takes a code minted for the browser holding testPending to
// gate/check, as that browser, and returns the route cookie it gets.
func redeem(t *testing.T, e *testEnv, code, host, uri string) string {
	t.Helper()
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	c := browserGet(uri + sep + wireCodeParam + "=" + code).withPending(testPending)
	c.host = host
	resp, _ := e.checkGate(t, c)
	return wantRedeemed(t, "redeeming at "+host+uri, resp, "https://"+host+uri)
}

func TestGateStartRefusesBadReturnURLs(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	for _, rd := range []string{
		"",
		"http://wiki.example.com/",
		"//evil.example/",
		"https://unprotected.example.com/",
		"https://evil.example/wiki.example.com",
		"https://wiki.example.com@evil.example/",
		"https://@wiki.example.com/",
		"https:wiki.example.com/",
		"https:/wiki.example.com/",
		"https://wiki.example.com\\@evil.example/",
		"javascript:alert(1)//wiki.example.com/",
		"https://wiki.example.com:8443/", // no other port is Outhaul's on this install
		"https://wiki.example.com:0443/",
		"https://[::1]/",
		"https://wiki.example.com/%zz",
		"https://wiKi.example.com/", // a Kelvin sign, which lower-cases to "k"
		"https://wiki.example.com, evil.example/",
	} {
		for _, tok := range []string{"", gate} {
			resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), tok, nil)
			wantPrivate(t, rd, resp)
			wantRefusal(t, rd, resp, http.StatusBadRequest, notProtectedCopy)
		}
	}
	if n := count(t, e, "gate_codes"); n != 0 {
		t.Errorf("%d codes minted for refused addresses", n)
	}

	resp := e.gateRequest(t, http.MethodGet, startPath("https://WIKI.example.com:443/x", testNonce), "", nil)
	if b := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(b, "Sign in to continue to") {
		t.Errorf("https://WIKI.example.com:443/x = %d, want the sign-in page", resp.StatusCode)
	}

	// Outhaul's own host is never a return address, whatever its rows say.
	if _, err := e.rawDB(t).Exec(`INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, protected, created_at)
		SELECT id, '127.0.0.1', '', 8080, '', '', 1, 1, created_at FROM apps WHERE name = 'wiki'`); err != nil {
		t.Fatal(err)
	}
	resp = e.gateRequest(t, http.MethodGet, startPath("https://127.0.0.1/", testNonce), gate, nil)
	wantRefusal(t, "Outhaul's own host", resp, http.StatusBadRequest, notProtectedCopy)
}

func TestGateStartRefusesOpenPathsOnAProtectedHost(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	for _, rd := range []string{
		"https://wiki.example.com/webhooks/x",
		"https://wiki.example.com/webhooks",
		"https://shop.example.com/",
		"https://shop.example.com",
		"https://shop.example.com/cart",
		"https://shop.example.com/ADMIN",
	} {
		resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), gate, nil)
		wantRefusal(t, rd, resp, http.StatusBadRequest, notProtectedCopy)
	}
	// A path a browser or Traefik could read differently from the gate is
	// refused for its spelling, and the page says so. It links to the app's
	// main page when that is protected (wiki's), and not otherwise (shop's).
	for rd, link := range map[string]string{
		"https://shop.example.com/admin/../cart":     "",
		"https://shop.example.com/admin/%2e%2e/cart": "",
		"https://shop.example.com/admin/%2E%2E/cart": "",
		"https://shop.example.com/admin%2f..%2fcart": "",
		"https://shop.example.com/admin%2F..%2Fcart": "",
		"https://shop.example.com/admin\\..\\cart":   "",
		"https://shop.example.com/admin%5c..%5ccart": "",
		"https://shop.example.com/admin/./users":     "",
		"https://shop.example.com//admin":            "",
		"https://shop.example.com/admin//users":      "",
		// Read undecoded, as Traefik may, this is the open row's path.
		"https://shop.example.com/%61dmin/users": "",
		"https://wiki.example.com//page":         "https://wiki.example.com/",
		"https://wiki.example.com/a/./b":         "https://wiki.example.com/",
		"https://wiki.example.com/a%2Fb":         "https://wiki.example.com/",
		"https://wiki.example.com/a\\b":          "https://wiki.example.com/",
	} {
		resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), gate, nil)
		wantPrivatePage(t, rd, resp)
		page := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, oddPathCopy) || strings.Contains(page, notProtectedCopy) {
			t.Errorf("%s: %d, want 400 saying %q:\n%s", rd, resp.StatusCode, oddPathCopy, page)
		}
		if got := attrAfter(page, `<a href="`); got != link {
			t.Errorf("%s: links to %q, want %q", rd, got, link)
		}
		if resp.Header.Get("Location") != "" || len(resp.Cookies()) != 0 {
			t.Errorf("%s: a refusal redirected to %q or set %q", rd, resp.Header.Get("Location"), resp.Header.Values("Set-Cookie"))
		}
	}
	if n := count(t, e, "gate_codes"); n != 0 {
		t.Fatalf("%d codes minted for open paths", n)
	}

	for rd, want := range map[string]string{
		"https://wiki.example.com/page":              "https://wiki.example.com/page",
		"https://wiki.example.com":                   "https://wiki.example.com/",
		"https://shop.example.com/admin/users?tab=1": "https://shop.example.com/admin/users?tab=1",
		"https://shop.example.com/admin/":            "https://shop.example.com/admin/",
		"https://wiki.example.com/webhook":           "https://wiki.example.com/webhook",
		"https://Wiki.Example.com./a%20b?q=%20x+y":   "https://wiki.example.com/a%20b?q=%20x+y",
	} {
		resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), gate, nil)
		resp.Body.Close()
		wantCodeRedirect(t, rd, resp, want)
	}
}

func TestGateStartShowsLoginNamingTheDestination(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	seedGateRows(t, e)
	rd := "https://wiki.example.com/page?x=1&y=2"
	resp := newBrowser(e).get(t, startPath(rd, testNonce))
	wantPrivatePage(t, "the sign-in page", resp)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /gate/start = %d: %s", resp.StatusCode, page)
	}
	for _, want := range []string{"Sign in to continue to", gateWikiHost, ssoButton, `action="/gate/sign-in"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	if got := attrAfter(page, `name="rd" value="`); got != rd {
		t.Errorf("hidden rd = %q, want %q", got, rd)
	}
	if got := attrAfter(page, `name="n" value="`); got != testNonce {
		t.Errorf("hidden n = %q, want %q", got, testNonce)
	}
	sso, err := url.Parse(attrAfter(page, `href="/auth/sso/start`))
	if err != nil || sso.Query().Get("rd") != rd || sso.Query().Get("n") != testNonce || len(sso.Query()) != 2 {
		t.Errorf("the SSO button's query = %q (%v), want rd and n", attrAfter(page, `href="/auth/sso/start`), err)
	}
	if strings.Contains(page, `action="/login"`) || strings.Contains(page, `href="/auth/sso/start"`) {
		t.Error("the page signs in to Outhaul instead of the route")
	}

	// A plain sign-in page is unchanged.
	plain := body(t, newBrowser(e).get(t, "/login"))
	if !strings.Contains(plain, `action="/login"`) || !strings.Contains(plain, `href="/auth/sso/start"`) || strings.Contains(plain, `name="rd"`) || strings.Contains(plain, "continue to") {
		t.Errorf("/login changed: %s", plain)
	}

	// With no SSO connected, there is no button.
	if err := e.store.ClearSSOSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if page := body(t, newBrowser(e).get(t, startPath(rd, testNonce))); strings.Contains(page, ssoButton) {
		t.Error("the SSO button shows with no provider connected")
	}
}

func TestPasswordSignInForRouteReturnsWithCode(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	rd := "https://wiki.example.com/page?x=1;y=2&" + wireCodeParam + "=stale"

	b := newBrowser(e)
	resp := b.post(t, "/gate/sign-in", signInForm("nora", "wrong password", rd, testNonce))
	wantPrivatePage(t, "a wrong password", resp)
	page := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "Invalid username or password.") {
		t.Errorf("a wrong password = %d: %s", resp.StatusCode, page)
	}
	for _, want := range []string{"Sign in to continue to", gateWikiHost} {
		if !strings.Contains(page, want) {
			t.Errorf("after a wrong password the page lacks %q", want)
		}
	}
	if attrAfter(page, `name="rd" value="`) != rd || attrAfter(page, `name="n" value="`) != testNonce {
		t.Errorf("after a wrong password the form carries rd %q and n %q", attrAfter(page, `name="rd" value="`), attrAfter(page, `name="n" value="`))
	}
	wantNoCookie(t, "a wrong password", resp)
	if n := count(t, e, "gate_sessions"); n != 0 {
		t.Errorf("a wrong password made %d gateway sessions", n)
	}

	resp = b.post(t, "/gate/sign-in", signInForm("nora", "supersecret", rd, testNonce))
	resp.Body.Close()
	wantPrivate(t, "a sign-in", resp)
	// The stale code is gone, and the ";" pair is kept as it was.
	code := wantCodeRedirect(t, "a sign-in", resp, "https://wiki.example.com/page?x=1;y=2")
	gc := setCookie(resp, wireGateCookie)
	if gc == nil || !isTokenHex(gc.Value) || !gc.HttpOnly || gc.SameSite != http.SameSiteLaxMode || gc.Path != "/" || gc.MaxAge != 8*3600 || gc.Secure || gc.Domain != "" {
		t.Errorf("gateway cookie = %+v; want a token, HttpOnly, Lax, Path /, 8 hours, not Secure on an http:// public URL", gc)
	}
	if n := count(t, e, "sessions"); n != 0 {
		t.Errorf("signing in for a route made %d Outhaul sessions", n)
	}
	if gc != nil {
		g, err := e.store.GetGateSession(context.Background(), gc.Value)
		if err != nil || g.Kind != core.SignInPassword || g.Display != "nora" {
			t.Errorf("gateway session = %+v, %v; want nora's, by password", g, err)
		}
	}
	route := redeem(t, e, code, gateWikiHost, "/page?x=1;y=2")
	got, _ := e.checkGate(t, browserGet("/page").with(route))
	wantPass(t, "nora's route cookie", got, "nora")
}

func TestGateSignInRefusesWhatLoginRefuses(t *testing.T) {
	e := gateSignInEnv(t)
	rd := "https://wiki.example.com/page"
	if _, err := e.store.CreatePasswordAccount(context.Background(), "unset", core.RoleSystemAdmin); err != nil { // no password yet
		t.Fatal(err)
	}
	sso := insertSSOUser(t, e, "dana@example.com", core.RoleSystemAdmin)
	hash, _ := HashPassword("supersecret")
	if _, err := e.rawDB(t).Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, sso); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unset", "nobody", "dana@example.com", "sso:dana@example.com"} {
		resp := newBrowser(e).post(t, "/gate/sign-in", signInForm(name, "supersecret", rd, testNonce))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("signing in as %q = %d, want 401", name, resp.StatusCode)
		}
	}

	// Every hashing slot taken: the person is told to wait, and keeps the
	// destination.
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	e.srv.hashWait = 20 * time.Millisecond
	for range cap(e.srv.hashSlots) {
		e.srv.hashSlots <- struct{}{}
	}
	resp := newBrowser(e).post(t, "/gate/sign-in", signInForm("nora", "supersecret", rd, testNonce))
	page := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, html.EscapeString(busyMessage)) || attrAfter(page, `name="n" value="`) != testNonce {
		t.Errorf("all slots busy = %d: %s", resp.StatusCode, page)
	}
	if n := count(t, e, "gate_sessions"); n != 0 {
		t.Errorf("%d gateway sessions made", n)
	}
}

// A password change that commits while the password is being checked leaves
// no gateway session behind, as it leaves no session behind at /login.
func TestGateSignInChecksThePasswordWasntChangedMeanwhile(t *testing.T) {
	e := gateSignInEnv(t)
	nora := passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	// Every hashing slot is taken, so the sign-in reads the account and then
	// waits; the change lands while it waits.
	e.srv.hashWait = 10 * time.Second
	for range cap(e.srv.hashSlots) {
		e.srv.hashSlots <- struct{}{}
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result)
	go func() {
		resp, err := newBrowser(e).client.PostForm(e.http.URL+"/gate/sign-in", signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
		done <- result{resp, err}
	}()
	time.Sleep(200 * time.Millisecond)
	if err := e.store.UpdateUserPassword(context.Background(), nora.ID, "$argon2id$changed", ""); err != nil {
		t.Fatal(err)
	}
	<-e.srv.hashSlots
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.resp.Body.Close()
	if r.resp.StatusCode != http.StatusUnauthorized || setCookie(r.resp, wireGateCookie) != nil {
		t.Errorf("a sign-in across a password change = %d (Location %q), want 401 and no cookie", r.resp.StatusCode, r.resp.Header.Get("Location"))
	}
	if n := count(t, e, "gate_sessions"); n != 0 {
		t.Errorf("%d gateway sessions made", n)
	}
}

func TestExistingGatewaySessionSkipsThePrompt(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	resp := e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com/page?x=1", testNonce), gate, nil)
	resp.Body.Close()
	wantPrivate(t, "a signed-in visit", resp)
	code := wantCodeRedirect(t, "a signed-in visit", resp, "https://wiki.example.com/page?x=1")
	for _, c := range resp.Cookies() {
		if c.Name != "outhaul_gate_laps" {
			t.Errorf("a signed-in visit set cookie %s, want only the lap count", c.Name)
		}
	}
	route := redeem(t, e, code, gateWikiHost, "/page?x=1")
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "the route cookie", got, "dana")

	// A gateway session that has ended is no session.
	if err := e.store.DeleteGateSession(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	resp = e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com/page", testNonce), gate, nil)
	if page := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(page, "Sign in to continue to") {
		t.Errorf("an ended gateway session = %d, want the sign-in page", resp.StatusCode)
	}
}

func TestCodeIsBoundToTheBrowserThatAsked(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")

	// Browser A visits the app and is sent to sign in.
	resp, _ := e.checkGate(t, browserGet("/page?x=1"))
	pc := setCookie(resp, wirePendingCookie)
	if pc == nil {
		t.Fatal("no pending cookie")
	}
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || to.Path != "/gate/start" {
		t.Fatalf("sent to %q", resp.Header.Get("Location"))
	}
	a := newBrowser(e)
	page := body(t, a.get(t, to.RequestURI()))
	if !strings.Contains(page, gateWikiHost) {
		t.Fatalf("A's sign-in page doesn't name the app: %s", page)
	}
	resp = a.post(t, "/gate/sign-in", signInForm("nora", "supersecret", to.Query().Get("rd"), to.Query().Get("n")))
	resp.Body.Close()
	code := wantCodeRedirect(t, "A signs in", resp, "https://wiki.example.com/page?x=1")
	uri := "/page?x=1&" + wireCodeParam + "=" + code

	// Browser B opens the same URL: no pending cookie, or one of its own.
	for _, pending := range []string{"", NewToken()} {
		resp, _ = e.checkGate(t, browserGet(uri).withPending(pending))
		if setCookie(resp, wireRouteCookie) != nil {
			t.Fatalf("browser B (pending %q) got a route cookie", pending)
		}
		wantSentToSignIn(t, "browser B", resp, "https://wiki.example.com/page?x=1")
	}

	// A's code still works in A.
	resp, _ = e.checkGate(t, browserGet(uri).withPending(pc.Value))
	route := wantRedeemed(t, "browser A", resp, "https://wiki.example.com/page?x=1")
	resp, _ = e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "A's route cookie", resp, "nora")

	// A missing or malformed n is refused, before anything is minted, even
	// for a browser that is signed in.
	codes := count(t, e, "gate_codes")
	rd := "https://wiki.example.com/page"
	for _, n := range []string{"", "short", strings.ToUpper(testNonce), testNonce[:63], testNonce + "0", testNonce[:63] + "g"} {
		resp := a.get(t, startPath(rd, n))
		wantPrivate(t, "n "+n, resp)
		wantRefusal(t, "/gate/start with n "+n, resp, http.StatusBadRequest, "sign-in link")
		resp = a.post(t, "/gate/sign-in", signInForm("nora", "supersecret", rd, n))
		wantRefusal(t, "/gate/sign-in with n "+n, resp, http.StatusBadRequest, "sign-in link")
	}
	resp = a.get(t, "/gate/start?rd="+url.QueryEscape(rd))
	wantRefusal(t, "no n at all", resp, http.StatusBadRequest, "sign-in link")

	// So is a return address with a fragment, empty or not.
	for _, rd := range []string{"https://wiki.example.com/page#top", "https://wiki.example.com/page#", "https://wiki.example.com/page?x=1#"} {
		resp := a.get(t, startPath(rd, testNonce))
		wantRefusal(t, rd, resp, http.StatusBadRequest, notProtectedCopy)
		resp = a.post(t, "/gate/sign-in", signInForm("nora", "supersecret", rd, testNonce))
		wantRefusal(t, "signing in for "+rd, resp, http.StatusBadRequest, notProtectedCopy)
	}
	if got := count(t, e, "gate_codes"); got != codes {
		t.Errorf("refusals minted %d codes", got-codes)
	}
}

func TestAdminLoginAlsoCoversRoutes(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	b := newBrowser(e)
	resp := b.post(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("POST /login = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if gc := setCookie(resp, wireGateCookie); gc == nil || gc.MaxAge != 8*3600 || !gc.HttpOnly {
		t.Errorf("POST /login set gateway cookie %+v, want one lasting 8 hours", gc)
	}
	resp = b.get(t, startPath("https://wiki.example.com/page", testNonce))
	resp.Body.Close()
	code := wantCodeRedirect(t, "after /login", resp, "https://wiki.example.com/page")
	route := redeem(t, e, code, gateWikiHost, "/page")
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "the admin's route cookie", got, "admin")
	if got := b.get(t, "/"); got.StatusCode != http.StatusOK {
		t.Errorf("the Outhaul session: GET / = %d, want 200", got.StatusCode)
	}

	// First-boot setup signs in through s.login, and covers routes too.
	e = gateSignInEnv(t)
	e.completeSetup(t)
	resp = e.get(t, startPath("https://wiki.example.com/page", testNonce))
	resp.Body.Close()
	wantCodeRedirect(t, "after first-boot setup", resp, "https://wiki.example.com/page")

	// So does finishing a setup link.
	ctx := context.Background()
	u, err := e.store.CreatePasswordAccount(ctx, "mina", core.RoleNone)
	if err != nil {
		t.Fatal(err)
	}
	link := NewToken()
	if err := e.store.CreateSetupLink(ctx, u.ID, link, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := newBrowser(e)
	resp = m.post(t, "/account/setup", url.Values{"token": {link}, "password": {"correct horse"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /account/setup = %d", resp.StatusCode)
	}
	resp = m.get(t, startPath("https://wiki.example.com/page", testNonce))
	resp.Body.Close()
	code = wantCodeRedirect(t, "after a setup link", resp, "https://wiki.example.com/page")
	route = redeem(t, e, code, gateWikiHost, "/page")
	got, _ = e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "mina's route cookie", got, "mina")
}

// The gateway cookie's name and Secure flag follow the public URL, not the
// request.
func TestGatewayCookieFollowsThePublicURL(t *testing.T) {
	for _, c := range []struct {
		public, proto, name string
		secure              bool
	}{
		{"https", "", wireGateCookieHTTPS, true},
		{"https", "http", wireGateCookieHTTPS, true},
		{"http", "", wireGateCookie, false},
		{"http", "https", wireGateCookie, false},
	} {
		e := newTestEnv(t)
		publicAt(e, c.public)
		passwordAdmin(t, e, "admin", "supersecret")
		req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"supersecret"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		resp, err := accountClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		gc := setCookie(resp, c.name)
		if gc == nil || gc.Secure != c.secure || gc.Path != "/" || gc.Domain != "" || !gc.HttpOnly || gc.SameSite != http.SameSiteLaxMode || gc.MaxAge != 8*3600 {
			t.Errorf("%s public URL, proto %q: gateway cookie %+v, want %s, Secure %v, Path /, HttpOnly, Lax, 8 hours (all: %q)",
				c.public, c.proto, gc, c.name, c.secure, resp.Header.Values("Set-Cookie"))
		}
	}
}

func TestSSORouteVisitCreatesNoAccount(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	fake.Identity.Groups = []string{"admins"}
	e := ssoLoginEnv(t, fake)
	e.srv.ssoEnv.RoleGroups = []core.GroupRole{{Group: "admins", Role: core.RoleSystemAdmin}}
	seedGateRows(t, e)
	ctx := context.Background()
	before, err := e.store.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions := count(t, e, "sessions")
	rd := "https://wiki.example.com/page?x=1"

	visit := func(t *testing.T, b *browser) *http.Response {
		t.Helper()
		resp := b.get(t, "/auth/sso/start?"+url.Values{"rd": {rd}, "n": {testNonce}}.Encode())
		resp.Body.Close()
		wantPrivate(t, "SSO start", resp)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc == nil || loc.Host != "sso.fake.invalid" {
			t.Fatalf("SSO start for a route = %d to %q, want 302 to the provider", resp.StatusCode, resp.Header.Get("Location"))
		}
		resp = b.callback(t, url.Values{"state": {loc.Query().Get("state")}, "code": {"C"}})
		resp.Body.Close()
		wantPrivate(t, "SSO callback", resp)
		return resp
	}

	b := newBrowser(e)
	resp := visit(t, b)
	code := wantCodeRedirect(t, "an SSO route visit", resp, rd)
	if _, ok := sessionCookieIn(resp); ok {
		t.Error("a route visit set an Outhaul session cookie")
	}
	gc := setCookie(resp, wireGateCookie)
	if gc == nil {
		t.Fatal("no gateway cookie")
	}
	g, err := e.store.GetGateSession(ctx, gc.Value)
	if err != nil || g.Kind != core.SignInSSO || g.Subject != core.SSOGateSubject(testIssuer, "sub-1") || g.Display != "dana@example.com" {
		t.Errorf("gateway session = %+v, %v; want dana's, by SSO", g, err)
	}
	after, err := e.store.ListUsers(ctx)
	if err != nil || len(after) != len(before) {
		t.Errorf("accounts: %d before, %d after (%v); want no new account", len(before), len(after), err)
	}
	if got := count(t, e, "sessions"); got != sessions {
		t.Errorf("Outhaul sessions: %d before, %d after", sessions, got)
	}
	route := redeem(t, e, code, gateWikiHost, "/page?x=1")
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "dana's route cookie", got, "dana@example.com")

	// Someone already signed in to Outhaul keeps their session, and isn't sent
	// to the dashboard first.
	pat := e.signInAs(t, "pat", core.RoleProjectViewer)
	b = newBrowser(e)
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: pat, Path: "/"}})
	resp = visit(t, b)
	wantCodeRedirect(t, "a signed-in person's SSO route visit", resp, rd)
	if _, ok := sessionCookieIn(resp); ok {
		t.Error("the route visit replaced the Outhaul session cookie")
	}
	if !hasSession(e, pat) {
		t.Error("the route visit ended the Outhaul session")
	}
	if got := b.get(t, "/"); got.StatusCode != http.StatusOK {
		t.Errorf("GET / with the Outhaul session afterwards = %d, want 200", got.StatusCode)
	}
}

// An SSO sign-in to Outhaul also covers routes, as the account's subject.
func TestSSOSignInAlsoCoversRoutes(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	seedGateRows(t, e)
	b := newBrowser(e)
	b.signIn(t)
	acct := ssoAccounts(t, e)[0]
	gc, ok := b.cookie(wireGateCookie)
	if !ok {
		t.Fatal("an SSO sign-in set no gateway cookie")
	}
	g, err := e.store.GetGateSession(context.Background(), gc.Value)
	if err != nil || g.Subject != acct.GateSubject() || g.Subject != core.SSOGateSubject(testIssuer, "sub-1") || g.Kind != core.SignInSSO || g.Display != "dana@example.com" {
		t.Errorf("gateway session = %+v, %v; want the account's", g, err)
	}
	resp := b.get(t, startPath("https://wiki.example.com/page", testNonce))
	resp.Body.Close()
	wantCodeRedirect(t, "after an SSO sign-in", resp, "https://wiki.example.com/page")
}

func TestSSOStartRefusesAnOverlongReturnURL(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	seedGateRows(t, e)
	base := "https://wiki.example.com/p?q="
	start := func(rd string) *http.Response {
		return newBrowser(e).get(t, "/auth/sso/start?"+url.Values{"rd": {rd}, "n": {testNonce}}.Encode())
	}

	long := base + strings.Repeat("a", 2001-len(base))
	if len(long) != 2001 {
		t.Fatal(len(long))
	}
	resp := start(long)
	wantRefusal(t, "a 2,001-byte return address", resp, http.StatusBadRequest, notProtectedCopy)
	if fake.LastAuthState != "" {
		t.Error("a sign-in was started")
	}

	// 2,000 bytes fits, even made of characters JSON could escape, and the
	// sign-in finishes.
	fits := base + strings.Repeat("&", 2000-len(base))
	b := newBrowser(e)
	resp = b.get(t, "/auth/sso/start?"+url.Values{"rd": {fits}, "n": {testNonce}}.Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a 2,000-byte return address = %d, want 302", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if strings.Contains(c.Name, ssoStateCookiePrefix) && len(c.Name)+len(c.Value) > 4096 {
			t.Errorf("the flow cookie is %d bytes, over a browser's 4 KB", len(c.Name)+len(c.Value))
		}
	}
	resp = b.callback(t, url.Values{"state": {fake.LastAuthState}, "code": {"C"}})
	resp.Body.Close()
	wantCodeRedirect(t, "a 2,000-byte return address", resp, fits)
}

func TestSSOStartRefusesBadReturnURL(t *testing.T) {
	fake := fakeIdentity("sub-1", "dana@example.com")
	e := ssoLoginEnv(t, fake)
	seedGateRows(t, e)
	pat := e.signInAs(t, "pat", core.RoleProjectViewer)
	for _, c := range []struct{ rd, n, copy string }{
		{"https://unprotected.example.com/", testNonce, notProtectedCopy},
		{"https://wiki.example.com/webhooks/x", testNonce, notProtectedCopy},
		{"https://wiki.example.com/page#x", testNonce, notProtectedCopy},
		{"https://wiki.example.com/page", "", "sign-in link"},
		{"https://wiki.example.com/page", "short", "sign-in link"},
		{"", testNonce, notProtectedCopy},
	} {
		for _, tok := range []string{"", pat} {
			resp := e.getAs(t, tok, "/auth/sso/start?"+url.Values{"rd": {c.rd}, "n": {c.n}}.Encode())
			wantRefusal(t, "rd "+c.rd+", n "+c.n, resp, http.StatusBadRequest, c.copy)
		}
	}
	if fake.LastAuthState != "" {
		t.Error("a sign-in was started")
	}
}

func TestSettingsChangeDuringRouteSignInLeavesNoGatewaySession(t *testing.T) {
	for _, route := range []bool{true, false} {
		e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
		seedGateRows(t, e)
		// The issuer changes as the gateway session is written: after Exchange
		// and the first check, before the second.
		if _, err := e.rawDB(t).Exec(`CREATE TRIGGER change_sso AFTER INSERT ON gate_sessions
			BEGIN UPDATE settings SET value = 'https://other.example.com' WHERE key = 'sso_issuer'; END`); err != nil {
			t.Fatal(err)
		}
		b := newBrowser(e)
		path := "/auth/sso/start"
		if route {
			path += "?" + url.Values{"rd": {"https://wiki.example.com/page"}, "n": {testNonce}}.Encode()
		}
		resp := b.get(t, path)
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if loc == nil || loc.Host != "sso.fake.invalid" {
			t.Fatalf("route %v: start = %d to %q", route, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp = b.callback(t, url.Values{"state": {loc.Query().Get("state")}, "code": {"C"}})
		resp.Body.Close()
		if l := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || strings.Contains(l, wireCodeParam) || strings.HasPrefix(l, "https://") || l == "/" {
			t.Errorf("route %v: callback = %d to %q, want a refusal", route, resp.StatusCode, l)
		}
		if n := count(t, e, "gate_sessions"); n != 0 {
			t.Errorf("route %v: %d gateway sessions left", route, n)
		}
		if n := count(t, e, "gate_codes"); n != 0 {
			t.Errorf("route %v: %d codes minted", route, n)
		}
		if gc, ok := b.cookie(wireGateCookie); ok {
			t.Errorf("route %v: the browser kept a gateway cookie %q", route, gc.Value)
		}
		var ssoSessions int
		if err := e.rawDB(t).QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id IN (SELECT id FROM users WHERE sign_in = 'sso')`).Scan(&ssoSessions); err != nil || ssoSessions != 0 {
			t.Errorf("route %v: SSO sessions = %d, %v", route, ssoSessions, err)
		}
	}
}

func TestNoGatewaySessionOffThePublicHost(t *testing.T) {
	e := newTestEnv(t)
	e.srv.publicURL = "https://outhaul.example.com"
	passwordAdmin(t, e, "admin", "supersecret")
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"supersecret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "10.0.0.5:8080"
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := sessionCookieIn(resp); !ok || resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("POST /login on 10.0.0.5:8080 = %d to %q, want signed in", resp.StatusCode, resp.Header.Get("Location"))
	}
	if setCookie(resp, wireGateCookie) != nil || setCookie(resp, wireGateCookieHTTPS) != nil {
		t.Errorf("a gateway cookie was set off the public host: %q", resp.Header.Values("Set-Cookie"))
	}
	if n := count(t, e, "gate_sessions"); n != 0 {
		t.Errorf("%d gateway sessions made off the public host", n)
	}

	// /gate/start there moves to the public URL first.
	seedGateRows(t, e)
	req, _ = http.NewRequest(http.MethodGet, e.http.URL+startPath("https://wiki.example.com/", testNonce), nil)
	req.Host = "10.0.0.5:8080"
	resp, err = accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "https://outhaul.example.com" + startPath("https://wiki.example.com/", testNonce) + "&moved=1"; resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
		t.Errorf("/gate/start on 10.0.0.5:8080 = %d to %q, want 302 to %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}
}

func TestSSOCallbackChecksReturnURLAgain(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	seedGateRows(t, e)
	forged := func(ret, nonce string) *http.Response {
		state := NewToken()
		c, _ := e.srv.ssoFlowCookie(state)
		v := encodeSSOFlow(ssoFlow{State: state, Nonce: NewToken(), Verifier: NewToken(), Expires: time.Now().Add(time.Minute), Return: ret, GateNonce: nonce})
		return rawCallback(t, e, c.Name, v, url.Values{"state": {state}, "code": {"C"}})
	}
	for _, c := range []struct{ ret, nonce string }{
		{"https://wiki.example.com/webhooks/x", testNonce},
		{"https://unprotected.example.com/", testNonce},
		{"https://wiki.example.com/page#x", testNonce},
		{"https://wiki.example.com/page", ""},
		{"https://wiki.example.com/page", "short"},
	} {
		resp := forged(c.ret, c.nonce)
		b := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(resp.Header.Get("Location"), wireCodeParam) {
			t.Errorf("a flow returning to %q with n %q = %d to %q (%s), want 400", c.ret, c.nonce, resp.StatusCode, resp.Header.Get("Location"), b)
		}
	}
	if n, m := count(t, e, "gate_sessions"), count(t, e, "gate_codes"); n != 0 || m != 0 {
		t.Errorf("edited flows left %d gateway sessions and %d codes", n, m)
	}

	// A row turned off between start and callback.
	b := newBrowser(e)
	resp := b.get(t, "/auth/sso/start?"+url.Values{"rd": {"https://wiki.example.com/page"}, "n": {testNonce}}.Encode())
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if _, err := e.rawDB(t).Exec(`UPDATE domains SET protected = 0 WHERE host = 'wiki.example.com' AND path = ''`); err != nil {
		t.Fatal(err)
	}
	resp = b.callback(t, url.Values{"state": {loc.Query().Get("state")}, "code": {"C"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(resp.Header.Get("Location"), wireCodeParam) {
		t.Errorf("a row turned off meanwhile: callback = %d to %q, want 400", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n, m := count(t, e, "gate_sessions"), count(t, e, "gate_codes"); n != 0 || m != 0 {
		t.Errorf("a row turned off meanwhile: %d gateway sessions and %d codes", n, m)
	}
}

func TestSSOFailureOnRouteVisitKeepsDestination(t *testing.T) {
	rd := "https://wiki.example.com/page?x=1"
	for _, c := range []struct {
		name, code string
		fail       func(f *sso.Fake) url.Values // the callback's query, given the state
	}{
		{"exchange error", "sso_failed", func(f *sso.Fake) url.Values {
			f.ExchangeErr = errors.New("invalid_grant")
			return url.Values{"code": {"C"}}
		}},
		{"no email", "sso_no_email", func(f *sso.Fake) url.Values {
			f.ExchangeErr = sso.ErrNoEmail
			return url.Values{"code": {"C"}}
		}},
		{"provider error", "sso_failed", func(f *sso.Fake) url.Values {
			return url.Values{"error": {"access_denied"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := fakeIdentity("sub-1", "dana@example.com")
			e := ssoLoginEnv(t, fake)
			seedGateRows(t, e)
			b := newBrowser(e)
			resp := b.get(t, "/auth/sso/start?"+url.Values{"rd": {rd}, "n": {testNonce}}.Encode())
			resp.Body.Close()
			q := c.fail(fake)
			q.Set("state", fake.LastAuthState)
			resp = b.callback(t, q)
			resp.Body.Close()
			want := "/gate/start?" + url.Values{"rd": {rd}, "n": {testNonce}, "error": {c.code}}.Encode()
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
				t.Fatalf("callback = %d to %q, want 303 to %q", resp.StatusCode, resp.Header.Get("Location"), want)
			}
			resp = b.get(t, want)
			page := body(t, resp)
			for _, s := range []string{html.EscapeString(ssoLoginErrors[c.code]), "Sign in to continue to", gateWikiHost, `action="/gate/sign-in"`} {
				if !strings.Contains(page, s) {
					t.Errorf("the page after a failed SSO sign-in lacks %q", s)
				}
			}
			if count(t, e, "gate_sessions") != 0 || len(ssoAccounts(t, e)) != 0 {
				t.Error("a failed sign-in left a gateway session or an account")
			}
		})
	}

	// With no usable flow there is nowhere to go back to but /login.
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	resp := newBrowser(e).callback(t, url.Values{"state": {NewToken()}, "code": {"C"}})
	resp.Body.Close()
	if resp.Header.Get("Location") != "/login?error=sso_failed" {
		t.Errorf("no flow: callback to %q, want /login?error=sso_failed", resp.Header.Get("Location"))
	}
}

// An unknown error code on the gate's page shows nothing.
func TestGateStartShowsOnlyKnownErrors(t *testing.T) {
	e := gateSignInEnv(t)
	resp := newBrowser(e).get(t, startPath("https://wiki.example.com/", testNonce)+"&error=%3Cscript%3Ealert(1)%3C%2Fscript%3E")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, "alert-error") || strings.Contains(page, "alert(1)") {
		t.Errorf("an unknown error = %d: %s", resp.StatusCode, page)
	}
}

func TestSignOutEndsRouteAccess(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	b := newBrowser(e)
	resp := b.post(t, "/gate/sign-in", signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
	resp.Body.Close()
	code := wantCodeRedirect(t, "sign-in", resp, "https://wiki.example.com/")
	route := redeem(t, e, code, gateWikiHost, "/")
	gc, ok := b.cookie(wireGateCookie)
	if !ok {
		t.Fatal("no gateway cookie after signing in")
	}

	// The page has a button, says SSO may sign you straight back in, and ends
	// nothing by itself.
	resp = b.get(t, "/gate/sign-out")
	page := body(t, resp)
	for _, want := range []string{`method="post" action="/gate/sign-out"`, "Sign out", "SSO", "straight back in"} {
		if !strings.Contains(page, want) {
			t.Errorf("GET /gate/sign-out lacks %q:\n%s", want, page)
		}
	}
	if _, err := e.store.RouteSessionUser(context.Background(), route, gateWikiHost); err != nil {
		t.Errorf("GET /gate/sign-out ended the route session: %v", err)
	}

	// A cross-origin POST is refused, and ends nothing.
	req, _ := http.NewRequest(http.MethodPost, e.http.URL+"/gate/sign-out", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(gc)
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site POST /gate/sign-out = %d, want 403", resp.StatusCode)
	}
	if _, err := e.store.RouteSessionUser(context.Background(), route, gateWikiHost); err != nil {
		t.Errorf("a cross-site POST ended the route session: %v", err)
	}

	resp = b.post(t, "/gate/sign-out", nil)
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "signed out") {
		t.Errorf("POST /gate/sign-out = %d: %s", resp.StatusCode, page)
	}
	if c := setCookie(resp, wireGateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("the gateway cookie wasn't cleared: %q", resp.Header.Values("Set-Cookie"))
	}
	if _, err := e.store.RouteSessionUser(context.Background(), route, gateWikiHost); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("RouteSessionUser after signing out: %v, want sql.ErrNoRows", err)
	}
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantSentToSignIn(t, "the old route cookie", got, "https://wiki.example.com/")
	// The next visit asks again.
	if page := body(t, e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com/", testNonce), gc.Value, nil)); !strings.Contains(page, "Sign in to continue to") {
		t.Error("the ended gateway session still signs in")
	}
}

func TestLogoutEndsRouteAccess(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	b := newBrowser(e)
	resp := b.post(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
	resp.Body.Close()
	resp = b.get(t, startPath("https://wiki.example.com/", testNonce))
	resp.Body.Close()
	route := redeem(t, e, wantCodeRedirect(t, "after /login", resp, "https://wiki.example.com/"), gateWikiHost, "/")
	sc, _ := b.cookie(sessionCookie)

	resp = b.post(t, "/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("POST /logout = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c := setCookie(resp, wireGateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("the gateway cookie wasn't cleared: %q", resp.Header.Values("Set-Cookie"))
	}
	if _, err := e.store.RouteSessionUser(context.Background(), route, gateWikiHost); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("RouteSessionUser after /logout: %v, want sql.ErrNoRows", err)
	}
	if count(t, e, "gate_sessions") != 0 {
		t.Error("a gateway session outlived /logout")
	}
	if hasSession(e, sc.Value) {
		t.Error("the Outhaul session outlived /logout")
	}
}

func TestRemovedAccountLosesRouteAccessAtOnce(t *testing.T) {
	e := gateSignInEnv(t)
	admin := e.signInAs(t, "root", core.RoleSystemAdmin)
	nora := passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	resp := newBrowser(e).post(t, "/gate/sign-in", signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
	resp.Body.Close()
	route := redeem(t, e, wantCodeRedirect(t, "sign-in", resp, "https://wiki.example.com/"), gateWikiHost, "/")
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "before", got, "nora")

	resp = e.postAs(t, admin, "/users/"+itoa(nora.ID)+"/delete", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	got, _ = e.checkGate(t, browserGet("/").with(route))
	if got.StatusCode != http.StatusFound || got.Header.Get(wireUserHeader) != "" {
		t.Errorf("after removing the account: %d with %q, want 302 to sign in", got.StatusCode, got.Header.Get(wireUserHeader))
	}
}

// Cross-origin protection covers the gate's sign-in form: a page on any other
// origin, an app on a sibling subdomain included, can't post to it.
func TestGateSignInRefusesCrossOriginPosts(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	form := signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce)
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://wiki.example.com"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/gate/sign-in", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" {
			t.Errorf("POST /gate/sign-in with %v = %d to %q, want 403", h, rec.Code, rec.Header().Get("Location"))
		}
	}
	if count(t, e, "gate_sessions") != 0 {
		t.Error("a cross-origin post made a gateway session")
	}
	// The form itself posts from the same origin.
	resp := newBrowser(e).post(t, "/gate/sign-in", form)
	resp.Body.Close()
	wantCodeRedirect(t, "a same-origin post", resp, "https://wiki.example.com/")
}

func TestGateSignInNeedsAPublicURL(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	e.srv.publicURL = ""
	resp := e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com/", testNonce), gate, nil)
	wantRefusal(t, "GET /gate/start", resp, http.StatusServiceUnavailable, gateUnavailableCopy)
	resp = e.gateRequest(t, http.MethodPost, "/gate/sign-in", "", signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
	wantRefusal(t, "POST /gate/sign-in", resp, http.StatusServiceUnavailable, gateUnavailableCopy)
	if n := count(t, e, "gate_codes"); n != 0 {
		t.Errorf("%d codes minted", n)
	}
}

// A browser that refuses the app host's cookies never keeps the pending or
// route cookie, so with a gateway session it would go round the gate and
// /gate/start for ever. The third lap to one host within two minutes stops at
// a page instead, with no code.
func TestGateStartStopsALoopForABrowserRefusingCookies(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	signedIn := func() *browser {
		b := newBrowser(e)
		u, _ := url.Parse(e.http.URL)
		b.client.Jar.SetCookies(u, []*http.Cookie{{Name: wireGateCookie, Value: gate, Path: "/"}})
		return b
	}
	// Each lap arrives from the gate with the hash of a new pending cookie,
	// as the app's host never kept the last one.
	lap := func(b *browser, rd string) *http.Response {
		return b.get(t, startPath(rd, nonceOf(NewToken())))
	}
	wiki, admin := "https://wiki.example.com/page", "https://shop.example.com/admin/"
	minted := func(what string, resp *http.Response, base string) {
		t.Helper()
		resp.Body.Close()
		wantCodeRedirect(t, what, resp, base)
	}

	b := signedIn()
	minted("lap 1", lap(b, wiki), wiki)
	minted("lap 2", lap(b, wiki), wiki)
	codes := count(t, e, "gate_codes")
	resp := lap(b, wiki)
	wantPrivatePage(t, "lap 3", resp)
	page := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("lap 3 = %d to %q, want a page instead", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.Contains(page, "cookies") || !strings.Contains(page, gateWikiHost) {
		t.Errorf("lap 3's page = %q, want it to say to accept cookies for %s", page, gateWikiHost)
	}
	if got := count(t, e, "gate_codes"); got != codes {
		t.Error("lap 3 minted a code")
	}
	// Trying again after the page starts a new count.
	minted("trying again", lap(b, wiki), wiki)

	// Laps to another host count apart.
	b = signedIn()
	minted("wiki 1", lap(b, wiki), wiki)
	minted("wiki 2", lap(b, wiki), wiki)
	minted("shop 1", lap(b, admin), admin)
	minted("wiki after shop", lap(b, wiki), wiki)

	// The count is kept on the Outhaul host for two minutes.
	resp = e.gateRequest(t, http.MethodGet, startPath(wiki, testNonce), gate, nil)
	resp.Body.Close()
	laps := setCookie(resp, "outhaul_gate_laps")
	if laps == nil || laps.MaxAge != 120 || !laps.HttpOnly || laps.Path != "/" || laps.SameSite != http.SameSiteLaxMode {
		t.Errorf("laps cookie = %+v, want HttpOnly, Lax, Path /, two minutes", laps)
	}
}

// On an install whose HTTPS entrypoint is published on another port, every
// URL the gate builds for an app's host carries that port: the configured
// one, never one a request named.
func TestGateUsesTheConfiguredHTTPSPort(t *testing.T) {
	e := gateSignInEnv(t)
	e.srv.tlsEnabled = true
	e.srv.SetHTTPSPort("8443")

	c := browserGet("/page?x=1")
	c.host = "wiki.example.com:9999"
	resp, _ := e.checkGate(t, c)
	to, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || to.Query().Get("rd") != "https://wiki.example.com:8443/page?x=1" {
		t.Errorf("sign-in redirect %d to %q, want rd https://wiki.example.com:8443/page?x=1", resp.StatusCode, resp.Header.Get("Location"))
	}
	code := mintGateCode(t, e, gateSignIn(t, e, "erin"), gateWikiHost, time.Minute)
	resp, _ = e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+code).withPending(testPending))
	wantRedeemed(t, "a code on 8443", resp, "https://wiki.example.com:8443/page?x=1")

	gate := gateSignIn(t, e, "dana")
	resp = e.gateRequest(t, http.MethodGet, startPath("https://WIKI.example.com:8443/page", testNonce), gate, nil)
	resp.Body.Close()
	code = wantCodeRedirect(t, "/gate/start on 8443", resp, "https://wiki.example.com:8443/page")
	// The code is for the host without its port.
	resp, _ = e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+code).withPending(testPending))
	wantRedeemed(t, "redeeming it", resp, "https://wiki.example.com:8443/page")
	for _, rd := range []string{"https://wiki.example.com/page", "https://wiki.example.com:443/page", "https://wiki.example.com:9999/page"} {
		resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), gate, nil)
		wantRefusal(t, rd+" on an 8443 install", resp, http.StatusBadRequest, notProtectedCopy)
	}
	// A path refused for its spelling links to the main page on that port.
	page := body(t, e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com:8443//page", testNonce), gate, nil))
	if got := attrAfter(page, `<a href="`); got != "https://wiki.example.com:8443/" {
		t.Errorf("an odd path on 8443 links to %q, want https://wiki.example.com:8443/", got)
	}

	// In tunnel mode visitors reach apps on 443, at the tunnel's edge.
	if err := e.store.SetCloudflareToken(context.Background(), "tunnel-token"); err != nil {
		t.Fatal(err)
	}
	resp, _ = e.checkGate(t, browserGet("/page?x=1"))
	if to, _ := url.Parse(resp.Header.Get("Location")); to == nil || to.Query().Get("rd") != "https://wiki.example.com/page?x=1" {
		t.Errorf("tunnel mode: sign-in redirect to %q, want rd without a port", resp.Header.Get("Location"))
	}
	resp = e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com:8443/page", testNonce), gate, nil)
	wantRefusal(t, "8443 in tunnel mode", resp, http.StatusBadRequest, notProtectedCopy)
	resp = e.gateRequest(t, http.MethodGet, startPath("https://wiki.example.com/page", testNonce), gate, nil)
	resp.Body.Close()
	wantCodeRedirect(t, "443 in tunnel mode", resp, "https://wiki.example.com/page")
}

// A request that isn't a GET or HEAD never carries a code legitimately: it
// answers 400, whether or not it has a route cookie.
func TestGateCheckRefusesACodeOnOtherMethods(t *testing.T) {
	e := newTestEnv(t)
	valid := routeCookieFor(t, e, "erin", gateWikiHost)
	code := mintGateCode(t, e, gateSignIn(t, e, "dana"), gateWikiHost, time.Minute)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		for _, cookie := range []string{"", valid} {
			resp, _ := e.checkGate(t, gateCall{host: gateWikiHost, uri: "/page?" + wireCodeParam + "=" + code, method: m, accept: browserAccept, cookie: cookie, pending: testPending})
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" || resp.Header.Get(wireUserHeader) != "" {
				t.Errorf("%s with a code (cookie %v) = %d to %q, want 400", m, cookie != "", resp.StatusCode, resp.Header.Get("Location"))
			}
			wantNoCookie(t, m, resp)
		}
	}
	// The code is unspent.
	resp, _ := e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+code).withPending(testPending))
	wantRedeemed(t, "the code on a GET", resp, "https://wiki.example.com/page")
}

// Refused codes and bad hosts are logged, but not once per request.
func TestGateCheckLogsRefusalsOnceAMinute(t *testing.T) {
	e := newTestEnv(t)
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	for range 3 {
		e.checkGate(t, gateCall{host: "evil.example/x", uri: "/", method: http.MethodGet, accept: browserAccept})
		e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+NewToken()))
		e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+NewToken()).withPending(testPending))
	}
	out := logged.String()
	for _, line := range []string{"isn't a host name", "no sign-in in progress", "it is unknown, used, expired"} {
		if n := strings.Count(out, line); n != 1 {
			t.Errorf("%q logged %d times for three requests, want 1:\n%s", line, n, out)
		}
	}
}

// Reusing a pending cookie doesn't extend its life: the gate sets one only
// when the browser has none it could have made.
func TestGateCheckDoesntRefreshAPendingCookie(t *testing.T) {
	e := newTestEnv(t)
	resp, _ := e.checkGate(t, browserGet("/a").withPending(testPending))
	if raw := resp.Header.Values("Set-Cookie"); len(raw) != 0 {
		t.Errorf("a reused pending cookie was sent again: %q", raw)
	}
	if want := signInURL("https://wiki.example.com/a", testNonce); resp.Header.Get("Location") != want {
		t.Errorf("Location %q, want %q", resp.Header.Get("Location"), want)
	}
}

// The public URL's host is the same with one trailing dot.
func TestOnPublicHostIgnoresATrailingDot(t *testing.T) {
	e := newTestEnv(t)
	for _, c := range []struct {
		public, host string
		want         bool
	}{
		{"https://outhaul.example.com", "outhaul.example.com.", true},
		{"https://outhaul.example.com", "OUTHAUL.example.com.:443", true},
		{"https://outhaul.example.com.", "outhaul.example.com", true},
		{"http://outhaul.example.com:8080", "outhaul.example.com.:8080", true},
		{"http://outhaul.example.com", "outhaul.example.com.:80", true},
		{"https://outhaul.example.com", "outhaul.example.com..", false},
		{"http://outhaul.example.com:8080", "outhaul.example.com.", false},
	} {
		e.srv.publicURL = c.public
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = c.host
		if got := e.srv.onPublicHost(r); got != c.want {
			t.Errorf("public URL %s, Host %q: onPublicHost = %v, want %v", c.public, c.host, got, c.want)
		}
	}
}

// A callback waiting for an exchange slot gives up after a while, so the
// person is told SSO is unavailable instead of waiting on a stuck provider.
func TestSSOExchangeWaitIsBounded(t *testing.T) {
	p := &gateProvider{entered: make(chan struct{}, 10), release: make(chan struct{})}
	e := ssoLoginEnv(t, p)
	e.srv.ssoExchangeWait = 50 * time.Millisecond
	done := make(chan struct{})
	for i := range maxConcurrentSSOExchanges {
		go func() {
			e.srv.handleSSOCallback(httptest.NewRecorder(), forgedCallback(e, "c"+itoa(int64(i))))
			done <- struct{}{}
		}()
		<-p.entered
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	e.srv.handleSSOCallback(rec, forgedCallback(e, "late"))
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the waiting callback took %v", took)
	}
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?error=sso_unavailable" {
		t.Errorf("the waiting callback = %d to %q, want sso_unavailable", rec.Code, rec.Header().Get("Location"))
	}
	close(p.release)
	for range maxConcurrentSSOExchanges {
		<-done
	}
}

// Nothing the sign-in side handles reaches the log: codes, gateway tokens,
// nonces, return addresses or passwords.
func TestGateSignInLogsNoSecrets(t *testing.T) {
	e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
	seedGateRows(t, e)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	rd := "https://wiki.example.com/secret-path?q=private"
	b := newBrowser(e)
	var issued []string
	keep := func(resp *http.Response) {
		for _, c := range resp.Cookies() {
			if c.Value != "" {
				issued = append(issued, c.Value)
			}
		}
		if l := resp.Header.Get("Location"); strings.Contains(l, wireCodeParam) {
			issued = append(issued, l[strings.LastIndex(l, "=")+1:])
		}
		resp.Body.Close()
	}
	keep(b.post(t, "/gate/sign-in", signInForm("nora", "wrong-password", rd, testNonce)))
	keep(b.post(t, "/gate/sign-in", signInForm("nora", "supersecret", rd, testNonce)))
	keep(b.get(t, startPath(rd, testNonce)))
	keep(b.get(t, startPath("https://wiki.example.com/webhooks/private", testNonce)))
	keep(b.get(t, startPath(rd, "bad-nonce-private")))
	e.srv.publicURL = ""
	keep(b.get(t, startPath(rd, testNonce)))
	e.srv.publicURL = e.http.URL
	keep(b.post(t, "/gate/sign-out", nil))

	out := logged.String()
	for _, secret := range append([]string{testNonce, "private", "secret-path", "supersecret", "wrong-password"}, issued...) {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q:\n%s", secret, out)
		}
	}
}

// The gate's pages name their URL in a Referer only to Outhaul's own origin.
// A browser that sends no Sec-Fetch-Site then posts their forms with an
// Origin equal to the page's, which cross-origin protection accepts. Under
// no-referrer it would send Origin: null, which is refused, as is any other
// origin.
func TestGateFormsPostFromTheirOwnOrigin(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
	b := newBrowser(e)
	for _, p := range []string{startPath("https://wiki.example.com/", testNonce), "/gate/sign-out"} {
		resp := b.get(t, p)
		resp.Body.Close()
		wantPrivatePage(t, "GET "+p, resp)
	}

	post := func(path, origin, gateTok string, form url.Values) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, e.http.URL+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		if gateTok != "" {
			req.AddCookie(&http.Cookie{Name: wireGateCookie, Value: gateTok})
		}
		resp, err := accountClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	form := signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce)

	resp := post("/gate/sign-in", e.http.URL, "", form)
	resp.Body.Close()
	wantCodeRedirect(t, "a sign-in carrying the page's Origin", resp, "https://wiki.example.com/")
	gc := setCookie(resp, wireGateCookie)
	if gc == nil {
		t.Fatal("no gateway cookie")
	}
	resp = post("/gate/sign-out", e.http.URL, gc.Value, nil)
	wantPrivatePage(t, "the signed-out page", resp)
	if page := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(page, "signed out") {
		t.Errorf("a sign-out carrying the page's Origin = %d: %s", resp.StatusCode, page)
	}
	if _, err := e.store.GetGateSession(context.Background(), gc.Value); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the gateway session outlived signing out: %v", err)
	}

	before := count(t, e, "gate_sessions")
	gate := gateSignIn(t, e, "dana")
	for _, origin := range []string{"null", "https://wiki.example.com", "http://evil.example"} {
		for _, path := range []string{"/gate/sign-in", "/gate/sign-out"} {
			resp := post(path, origin, gate, form)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
				t.Errorf("POST %s with Origin %s = %d to %q, want 403", path, origin, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
	}
	if got := count(t, e, "gate_sessions"); got != before+1 {
		t.Errorf("refused posts changed the gateway sessions: %d, want %d", got, before+1)
	}
}

// Tabs that share one pending cookie bring the same n, and each still signs
// in. Only an arrival with a new n, as from a browser that never keeps the
// pending cookie, counts toward the loop guard.
func TestGateStartLoopGuardSparesTabs(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	b := newBrowser(e)
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: wireGateCookie, Value: gate, Path: "/"}})
	wiki := "https://wiki.example.com/page"
	for i := range 5 {
		resp := b.get(t, startPath(wiki, testNonce))
		resp.Body.Close()
		wantCodeRedirect(t, "tab "+itoa(int64(i+1)), resp, wiki)
	}
	// A new n after them, as when the pending cookie expired and was made
	// again, is the second lap, not the sixth; its own tabs don't count either.
	renewed := nonceOf(NewToken())
	for i := range 3 {
		resp := b.get(t, startPath(wiki, renewed))
		resp.Body.Close()
		wantCodeRedirect(t, "a renewed pending cookie, tab "+itoa(int64(i+1)), resp, wiki)
	}
	// A third n in a row is the third lap.
	resp := b.get(t, startPath(wiki, nonceOf(NewToken())))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("a third new n = %d to %q, want the cookies page", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Two tabs, each holding its own pending cookie, are sent to /gate/start one
// after the other: each code works in its own tab. Minting the second
// doesn't void the first.
func TestGateCodesOfTwoTabsBothRedeem(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	b := newBrowser(e)
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: wireGateCookie, Value: gate, Path: "/"}})
	wiki := "https://wiki.example.com/page"
	pending := []string{NewToken(), NewToken()}
	var codes []string
	for i, p := range pending {
		resp := b.get(t, startPath(wiki, nonceOf(p)))
		resp.Body.Close()
		codes = append(codes, wantCodeRedirect(t, "tab "+itoa(int64(i+1)), resp, wiki))
	}
	for i, p := range pending {
		resp, _ := e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+codes[i]).withPending(p))
		route := wantRedeemed(t, "tab "+itoa(int64(i+1)), resp, wiki)
		resp, _ = e.checkGate(t, browserGet("/").with(route))
		wantPass(t, "tab "+itoa(int64(i+1))+"'s route cookie", resp, "dana")
	}
}

// redirectWithCode refuses a malformed nonce itself, whatever its caller
// checked, and mints nothing: a code bound to no browser's pending cookie
// could never be redeemed, or could be by the wrong one.
func TestRedirectWithCodeRefusesABadNonce(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	before := count(t, e, "gate_codes")
	for _, n := range []string{"", "short", strings.ToUpper(testNonce), testNonce + "0"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/gate/start", nil)
		e.srv.redirectWithCode(rec, req, gate, "https://wiki.example.com/page", n)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("nonce %q: %d to %q, want 400 and no redirect", n, rec.Code, rec.Header().Get("Location"))
		}
	}
	if got := count(t, e, "gate_codes"); got != before {
		t.Errorf("bad nonces minted %d codes", got-before)
	}
}

// A return address that already carries a code (an old link, or a loop) is
// sent back with only the new one: the old is stripped, the rest kept.
func TestCodeRedirectReplacesAnOldCode(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	b := newBrowser(e)
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: wireGateCookie, Value: gate, Path: "/"}})
	old := NewToken()
	for _, rd := range []string{
		"https://wiki.example.com/page?x=1&" + wireCodeParam + "=" + old,
		"https://wiki.example.com/page?" + wireCodeParam + "=" + old + "&x=1",
	} {
		resp := b.get(t, startPath(rd, testNonce))
		resp.Body.Close()
		if code := wantCodeRedirect(t, rd, resp, "https://wiki.example.com/page?x=1"); code == old {
			t.Errorf("%s: the old code came back", rd)
		}
	}
}

// /gate/start mints a code only for a navigation. A HEAD or a prefetch with
// a live gateway session is answered without one, and the code a real
// navigation got still works.
func TestGateStartMintsOnlyForANavigation(t *testing.T) {
	e := gateSignInEnv(t)
	gate := gateSignIn(t, e, "dana")
	rd := "https://wiki.example.com/page"
	resp := e.gateRequest(t, http.MethodGet, startPath(rd, testNonce), gate, nil)
	resp.Body.Close()
	code := wantCodeRedirect(t, "a navigation", resp, rd)
	codes := count(t, e, "gate_codes")

	ask := func(method string, header map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, e.http.URL+startPath(rd, testNonce), nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		req.AddCookie(&http.Cookie{Name: wireGateCookie, Value: gate})
		resp, err := accountClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp = ask(http.MethodHead, nil)
	if b := body(t, resp); resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "" || b != "" || len(resp.Cookies()) != 0 {
		t.Errorf("HEAD = %d to %q, %d bytes, cookies %q; want 200 and nothing else", resp.StatusCode, resp.Header.Get("Location"), len(b), resp.Header.Values("Set-Cookie"))
	}
	for _, h := range []map[string]string{
		{"Sec-Purpose": "prefetch"},
		{"Sec-Purpose": "prefetch;prerender"},
		{"Purpose": "prefetch"},
		{"X-Moz": "prefetch"},
	} {
		resp := ask(http.MethodGet, h)
		if b := body(t, resp); resp.StatusCode != http.StatusNoContent || resp.Header.Get("Location") != "" || b != "" || len(resp.Cookies()) != 0 {
			t.Errorf("a prefetch (%v) = %d to %q, %d bytes, cookies %q; want 204 and nothing else", h, resp.StatusCode, resp.Header.Get("Location"), len(b), resp.Header.Values("Set-Cookie"))
		}
	}
	if got := count(t, e, "gate_codes"); got != codes {
		t.Errorf("codes: %d before, %d after; a HEAD or prefetch minted or spent one", codes, got)
	}
	route := redeem(t, e, code, gateWikiHost, "/page")
	got, _ := e.checkGate(t, browserGet("/").with(route))
	wantPass(t, "the navigation's code", got, "dana")
}

// heldGateway gives b a gateway session, and a route session on wiki that
// came from it, as an earlier sign-in in b would have left them.
func heldGateway(t *testing.T, e *testEnv, b *browser) (gate, route string) {
	t.Helper()
	gate = gateSignIn(t, e, "earlier")
	code := mintGateCode(t, e, gate, gateWikiHost, time.Minute)
	route = NewToken()
	if _, err := e.store.RedeemGateCode(context.Background(), code, gateWikiHost, nonceOf(testPending), route, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.http.URL)
	b.client.Jar.SetCookies(u, []*http.Cookie{{Name: e.srv.gateCookieName(), Value: gate, Path: "/"}})
	return gate, route
}

// wantGatewayReplaced checks b now holds a new, live gateway session, and
// the one it held before, with its route session, no longer works.
func wantGatewayReplaced(t *testing.T, e *testEnv, b *browser, gate, route string) {
	t.Helper()
	ctx := context.Background()
	if gc, ok := b.cookie(e.srv.gateCookieName()); !ok || gc.Value == gate {
		t.Error("the browser didn't get a new gateway cookie")
	} else if _, err := e.store.GetGateSession(ctx, gc.Value); err != nil {
		t.Errorf("the new gateway session: %v", err)
	}
	if _, err := e.store.GetGateSession(ctx, gate); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the earlier gateway session still works (err %v)", err)
	}
	if _, err := e.store.RouteSessionUser(ctx, route, gateWikiHost); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the earlier route session still works (err %v)", err)
	}
}

// Signing in again in one browser, by any way in, ends the gateway session
// the browser held, so the next person on a shared browser doesn't pass
// protected routes as the last one.
func TestSigningInAgainEndsTheEarlierGatewaySession(t *testing.T) {
	t.Run("/login", func(t *testing.T) {
		e := gateSignInEnv(t)
		passwordAdmin(t, e, "admin", "supersecret")
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		resp := b.post(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("POST /login = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		wantGatewayReplaced(t, e, b, gate, route)
	})
	t.Run("/gate/sign-in", func(t *testing.T) {
		e := gateSignInEnv(t)
		passwordAccount(t, e, "nora", core.RoleNone, "supersecret")
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		resp := b.post(t, "/gate/sign-in", signInForm("nora", "supersecret", "https://wiki.example.com/", testNonce))
		resp.Body.Close()
		wantCodeRedirect(t, "POST /gate/sign-in", resp, "https://wiki.example.com/")
		wantGatewayReplaced(t, e, b, gate, route)
	})
	t.Run("setup link", func(t *testing.T) {
		e := gateSignInEnv(t)
		e.completeSetup(t)
		ctx := context.Background()
		u, err := e.store.CreatePasswordAccount(ctx, "mina", core.RoleNone)
		if err != nil {
			t.Fatal(err)
		}
		link := NewToken()
		if err := e.store.CreateSetupLink(ctx, u.ID, link, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		resp := b.post(t, "/account/setup", url.Values{"token": {link}, "password": {"correct horse"}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /account/setup = %d", resp.StatusCode)
		}
		wantGatewayReplaced(t, e, b, gate, route)
	})
	t.Run("SSO", func(t *testing.T) {
		e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		b.signIn(t)
		wantGatewayReplaced(t, e, b, gate, route)
	})
	t.Run("SSO route visit", func(t *testing.T) {
		fake := fakeIdentity("sub-1", "dana@example.com")
		e := ssoLoginEnv(t, fake)
		seedGateRows(t, e)
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		resp := b.get(t, "/auth/sso/start?"+url.Values{"rd": {"https://wiki.example.com/"}, "n": {testNonce}}.Encode())
		resp.Body.Close()
		resp = b.callback(t, url.Values{"state": {fake.LastAuthState}, "code": {"C"}})
		resp.Body.Close()
		wantCodeRedirect(t, "an SSO route visit", resp, "https://wiki.example.com/")
		wantGatewayReplaced(t, e, b, gate, route)
	})
}

// A refused SSO sign-in leaves the gateway cookie the browser already had,
// and its session, as they were.
func TestRefusedSSOSignInKeepsTheEarlierGatewaySession(t *testing.T) {
	for _, routeVisit := range []bool{true, false} {
		e := ssoLoginEnv(t, fakeIdentity("sub-1", "dana@example.com"))
		seedGateRows(t, e)
		b := newBrowser(e)
		gate, route := heldGateway(t, e, b)
		// The issuer changes as the new gateway session is written, so the
		// sign-in is refused at its second check.
		if _, err := e.rawDB(t).Exec(`CREATE TRIGGER change_sso AFTER INSERT ON gate_sessions
			BEGIN UPDATE settings SET value = 'https://other.example.com' WHERE key = 'sso_issuer'; END`); err != nil {
			t.Fatal(err)
		}
		path := "/auth/sso/start"
		if routeVisit {
			path += "?" + url.Values{"rd": {"https://wiki.example.com/page"}, "n": {testNonce}}.Encode()
		}
		resp := b.get(t, path)
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if loc == nil || loc.Host != "sso.fake.invalid" {
			t.Fatalf("route %v: start = %d to %q", routeVisit, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp = b.callback(t, url.Values{"state": {loc.Query().Get("state")}, "code": {"C"}})
		resp.Body.Close()
		if l := resp.Header.Get("Location"); strings.Contains(l, wireCodeParam) || l == "/" {
			t.Fatalf("route %v: callback to %q, want a refusal", routeVisit, l)
		}
		if gc, ok := b.cookie(wireGateCookie); !ok || gc.Value != gate {
			t.Errorf("route %v: the browser's gateway cookie was replaced or cleared", routeVisit)
		}
		if _, err := e.store.GetGateSession(context.Background(), gate); err != nil {
			t.Errorf("route %v: the earlier gateway session: %v", routeVisit, err)
		}
		if _, err := e.store.RouteSessionUser(context.Background(), route, gateWikiHost); err != nil {
			t.Errorf("route %v: the earlier route session: %v", routeVisit, err)
		}
		if n := count(t, e, "gate_sessions"); n != 1 {
			t.Errorf("route %v: %d gateway sessions, want only the earlier one", routeVisit, n)
		}
	}
}

// A start moved to the public URL that arrives elsewhere all the same is
// logged, but not once per request.
func TestMovedSignInsLogOnceAMinute(t *testing.T) {
	e := newTestEnv(t)
	e.srv.publicURL = "https://outhaul.example.com"
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	for range 3 {
		for _, p := range []string{startPath("https://wiki.example.com/", testNonce) + "&moved=1", "/auth/sso/start?moved=1"} {
			req, _ := http.NewRequest(http.MethodGet, e.http.URL+p, nil)
			req.Host = "10.0.0.5:8080"
			resp, err := accountClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
	}
	out := logged.String()
	for _, line := range []string{"gate: sign-in page refused", "sso: sign-in refused: it was sent on"} {
		if n := strings.Count(out, line); n != 1 {
			t.Errorf("%q logged %d times for three requests, want 1:\n%s", line, n, out)
		}
	}
}

// /logout that can't end the gateway session says so, and ends nothing: the
// person can try again. A delete that fails once is tried again first.
func TestLogoutEndsNothingWhenTheGatewaySessionCantBeEnded(t *testing.T) {
	e := gateSignInEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	b := newBrowser(e)
	resp := b.post(t, "/login", url.Values{"username": {"admin"}, "password": {"supersecret"}})
	resp.Body.Close()
	sc, _ := b.cookie(sessionCookie)
	gc, ok := b.cookie(wireGateCookie)
	if sc == nil || !ok {
		t.Fatal("not signed in")
	}
	db := e.rawDB(t)
	if _, err := db.Exec(`CREATE TRIGGER keep_gate BEFORE DELETE ON gate_sessions BEGIN SELECT RAISE(ABORT, 'kept'); END`); err != nil {
		t.Fatal(err)
	}
	resp = b.post(t, "/logout", nil)
	page := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "Signing out didn't work") || resp.Header.Get("Location") != "" {
		t.Errorf("POST /logout with the delete failing = %d to %q: %s", resp.StatusCode, resp.Header.Get("Location"), page)
	}
	if raw := resp.Header.Values("Set-Cookie"); len(raw) != 0 {
		t.Errorf("a failed sign-out cleared cookies: %q", raw)
	}
	if !hasSession(e, sc.Value) {
		t.Error("a failed sign-out ended the Outhaul session")
	}
	if _, err := e.store.GetGateSession(context.Background(), gc.Value); err != nil {
		t.Errorf("the gateway session: %v", err)
	}

	if _, err := db.Exec(`DROP TRIGGER keep_gate`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE gate_fail (n INTEGER);
		INSERT INTO gate_fail VALUES (1);
		CREATE TRIGGER fail_once BEFORE DELETE ON gate_sessions WHEN (SELECT n FROM gate_fail) > 0
		BEGIN UPDATE gate_fail SET n = n - 1; SELECT RAISE(FAIL, 'once'); END`); err != nil {
		t.Fatal(err)
	}
	resp = b.post(t, "/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("POST /logout with one failed delete = %d to %q, want 303 to /login", resp.StatusCode, resp.Header.Get("Location"))
	}
	var left int
	if err := db.QueryRow(`SELECT n FROM gate_fail`).Scan(&left); err != nil || left != 0 {
		t.Errorf("the delete didn't fail first (n = %d, %v)", left, err)
	}
	if _, err := e.store.GetGateSession(context.Background(), gc.Value); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the gateway session outlived /logout: %v", err)
	}
	if hasSession(e, sc.Value) {
		t.Error("the Outhaul session outlived /logout")
	}
}
