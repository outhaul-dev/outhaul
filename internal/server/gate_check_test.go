package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// The names on the wire are spelled out here rather than taken from the
// constants, so renaming a constant can't quietly change what browsers,
// Traefik and apps see.
const (
	gateWikiHost      = "wiki.example.com"
	gateDocsHost      = "docs.example.com"
	browserAccept     = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	wireCodeParam     = "__outhaul_code"
	wireRouteCookie   = "__Host-outhaul_route"
	wirePendingCookie = "__Host-outhaul_pending"
	wireUserHeader    = "X-Outhaul-User"
)

// testPending is a pending cookie value, as the gate makes them: 32 random
// bytes in hex. mintGateCode binds codes to it unless told otherwise.
const testPending = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// gateCall is one visitor request as Traefik's forward auth describes it to
// the gate: the forwarded headers it sets, plus the browser's own headers,
// which it copies.
type gateCall struct {
	host, uri, method, accept string
	cookie                    string            // the route cookie's value; "" sends none
	pending                   string            // the pending cookie's value; "" sends none
	extra                     map[string]string // any other header; set last, so it can override
}

// browserGet is a page navigation to uri on wiki.example.com.
func browserGet(uri string) gateCall {
	return gateCall{host: gateWikiHost, uri: uri, method: http.MethodGet, accept: browserAccept}
}

func (c gateCall) with(cookie string) gateCall { c.cookie = cookie; return c }

func (c gateCall) withPending(p string) gateCall { c.pending = p; return c }

// checkGate asks the gate about c, through the server's full handler and over
// HTTP, as Traefik does. It returns the answer with its body read.
func (e *testEnv) checkGate(t *testing.T, c gateCall) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+"/gate/check", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	set := func(k, v string) {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	set("X-Forwarded-Host", c.host)
	set("X-Forwarded-Uri", c.uri)
	set("X-Forwarded-Method", c.method)
	set("X-Forwarded-Proto", "https")
	set("Accept", c.accept)
	for k, v := range c.extra {
		req.Header.Set(k, v)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: wireRouteCookie, Value: c.cookie})
	}
	if c.pending != "" {
		req.AddCookie(&http.Cookie{Name: wirePendingCookie, Value: c.pending})
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatalf("GET /gate/check: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(b)
}

// nonceOf is the nonce a pending cookie stands for in the sign-in URL and on
// a code: the SHA-256 hex of its value. It is computed here, not borrowed
// from the gate, so the tests pin it.
func nonceOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// gateSignIn starts a gateway session for display and returns its raw token.
func gateSignIn(t *testing.T, e *testEnv, display string) string {
	t.Helper()
	tok := NewToken()
	if err := e.store.CreateGateSession(context.Background(), tok, core.GateSession{
		Subject: "user:" + display, Kind: core.SignInPassword, Display: display, ExpiresAt: time.Now().Add(8 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateGateSession: %v", err)
	}
	return tok
}

// mintGateCode stores a one-time code that carries gateTok to host, in the
// browser holding testPending, expiring after ttl.
func mintGateCode(t *testing.T, e *testEnv, gateTok, host string, ttl time.Duration) string {
	t.Helper()
	return mintGateCodeFor(t, e, gateTok, host, nonceOf(testPending), ttl)
}

// mintGateCodeFor is mintGateCode for the browser whose pending cookie hashes
// to nonce.
func mintGateCodeFor(t *testing.T, e *testEnv, gateTok, host, nonce string, ttl time.Duration) string {
	t.Helper()
	code := NewToken()
	if err := e.store.CreateGateCode(context.Background(), code, gateTok, host, nonce, time.Now().Add(ttl)); err != nil {
		t.Fatalf("CreateGateCode: %v", err)
	}
	return code
}

// routeCookieFor signs display in to host and returns the route cookie's value.
func routeCookieFor(t *testing.T, e *testEnv, display, host string) string {
	t.Helper()
	code := mintGateCode(t, e, gateSignIn(t, e, display), host, time.Minute)
	tok := NewToken()
	if _, err := e.store.RedeemGateCode(context.Background(), code, host, nonceOf(testPending), tok, time.Now().Add(8*time.Hour)); err != nil {
		t.Fatalf("RedeemGateCode: %v", err)
	}
	return tok
}

// signInURL is where a browser bound for returnURL is sent to sign in, from
// the browser whose pending cookie hashes to nonce.
func signInURL(returnURL, nonce string) string {
	return "https://slip.example.com/gate/start?n=" + nonce + "&rd=" + url.QueryEscape(returnURL)
}

// setCookie returns the cookie named name that resp sets, or nil.
func setCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// isTokenHex reports whether v looks like NewToken's output.
func isTokenHex(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && strings.ToLower(v) == v
}

// wantSignIn checks the answer sends the browser to sign in, to come back to
// returnURL, with its pending cookie named in the URL by its hash. A browser
// that sent a plausible pending cookie keeps it, and is sent no cookie at
// all, so reusing one doesn't extend its life. Any other browser is sent a
// new pending cookie and no other. It returns the pending cookie's value.
func wantSignIn(t *testing.T, what string, resp *http.Response, returnURL, sent string) string {
	t.Helper()
	if u := resp.Header.Get(wireUserHeader); u != "" {
		t.Errorf("%s: a redirect carries %s %q", what, wireUserHeader, u)
	}
	if isTokenHex(sent) {
		wantNoCookie(t, what+", reusing the pending cookie", resp)
		if want := signInURL(returnURL, nonceOf(sent)); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", what, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		return sent
	}
	if raw := resp.Header.Values("Set-Cookie"); len(raw) != 1 {
		t.Errorf("%s: Set-Cookie = %q, want only the pending cookie", what, raw)
	}
	p := setCookie(resp, wirePendingCookie)
	if p == nil {
		t.Errorf("%s: %d to %q sets no pending cookie", what, resp.StatusCode, resp.Header.Get("Location"))
		return ""
	}
	wantPendingAttrs(t, what, resp, p)
	if p.Value == sent {
		t.Errorf("%s: an implausible pending cookie %q was kept", what, sent)
	}
	if want := signInURL(returnURL, nonceOf(p.Value)); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
		t.Errorf("%s: %d to %q, want 302 to %q", what, resp.StatusCode, resp.Header.Get("Location"), want)
	}
	return p.Value
}

// wantPendingAttrs checks a pending cookie is set as the design says: a new
// token, host-only, Secure, HttpOnly, Lax, for ten minutes.
func wantPendingAttrs(t *testing.T, what string, resp *http.Response, p *http.Cookie) {
	t.Helper()
	if !isTokenHex(p.Value) {
		t.Errorf("%s: pending cookie value %q, want 64 lower-case hex characters", what, p.Value)
	}
	if p.Path != "/" || !p.Secure || !p.HttpOnly || p.SameSite != http.SameSiteLaxMode || p.MaxAge != 600 {
		t.Errorf("%s: pending cookie Path=%q Secure=%v HttpOnly=%v SameSite=%v Max-Age=%d, want /, true, true, Lax, 600",
			what, p.Path, p.Secure, p.HttpOnly, p.SameSite, p.MaxAge)
	}
	for _, raw := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(raw, wirePendingCookie+"=") && (p.Domain != "" || strings.Contains(strings.ToLower(raw), "domain=")) {
			t.Errorf("%s: the pending cookie names a domain: %q", what, raw)
		}
	}
}

// wantRedeemed checks the answer is a 302 to exactly to that sets a new route
// cookie and clears the pending one, and returns the route cookie's value.
func wantRedeemed(t *testing.T, what string, resp *http.Response, to string) string {
	t.Helper()
	wantRedirect(t, what, resp, to)
	if raw := resp.Header.Values("Set-Cookie"); len(raw) != 2 {
		t.Errorf("%s: Set-Cookie = %q, want the route cookie and the pending cookie's removal", what, raw)
	}
	p := setCookie(resp, wirePendingCookie)
	if p == nil || p.Value != "" || p.MaxAge >= 0 || p.Path != "/" || !p.Secure || !p.HttpOnly || p.SameSite != http.SameSiteLaxMode {
		t.Errorf("%s: pending cookie %+v, want it cleared with Path=/, Secure, HttpOnly, SameSite=Lax", what, p)
	}
	r := setCookie(resp, wireRouteCookie)
	if r == nil {
		t.Errorf("%s: no route cookie", what)
		return ""
	}
	return r.Value
}

// wantRedirect checks the answer is a 302 to exactly to.
func wantRedirect(t *testing.T, what string, resp *http.Response, to string) {
	t.Helper()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != to {
		t.Errorf("%s: %d to %q, want 302 to %q", what, resp.StatusCode, resp.Header.Get("Location"), to)
	}
}

func wantNoCookie(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if sc := resp.Header.Values("Set-Cookie"); len(sc) > 0 {
		t.Errorf("%s: sets a cookie: %q", what, sc)
	}
}

// wantPass checks the gate lets the request through as display.
func wantPass(t *testing.T, what string, resp *http.Response, display string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s: status %d (Location %q), want 200", what, resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := resp.Header.Get(wireUserHeader); got != display {
		t.Errorf("%s: %s = %q, want %q", what, wireUserHeader, got, display)
	}
}

// want401 checks the answer is the short plain refusal scripts get.
func want401(t *testing.T, what string, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("%s: status %d (Location %q), want 401", what, resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("%s: a 401 has Location %q", what, loc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("%s: Content-Type %q, want text/plain", what, ct)
	}
	if b := strings.TrimSpace(body); b == "" || len(b) > 100 {
		t.Errorf("%s: body %q, want a short sentence", what, body)
	}
	wantNoCookie(t, what, resp)
}

func TestGateCheckPassesAValidCookie(t *testing.T) {
	e := newTestEnv(t)
	tok := routeCookieFor(t, e, "dana@example.com", gateWikiHost)

	// Traefik copies the browser's own headers onto the check, fetch metadata
	// included. The check is a GET, so cross-origin protection lets it by.
	c := browserGet("/page?x=1").with(tok)
	c.extra = map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}
	resp, _ := e.checkGate(t, c)
	wantPass(t, "a page", resp, "dana@example.com")
	wantNoCookie(t, "a page", resp)
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("a pass has Location %q", loc)
	}

	// Not only page navigations: a script's call with the cookie passes too.
	resp, _ = e.checkGate(t, gateCall{host: gateWikiHost, uri: "/api/items", method: http.MethodPost, accept: "application/json", cookie: tok})
	wantPass(t, "a script's POST", resp, "dana@example.com")
}

func TestGateCheckNormalizesHost(t *testing.T) {
	e := newTestEnv(t)
	tok := routeCookieFor(t, e, "dana", gateWikiHost)
	for _, host := range []string{"WIKI.example.com:443", "wiki.example.com:443", "Wiki.Example.Com", "wiki.example.com.", "WIKI.EXAMPLE.COM.:443"} {
		c := browserGet("/page").with(tok)
		c.host = host
		resp, _ := e.checkGate(t, c)
		wantPass(t, host, resp, "dana")
	}

	// The way back is built from the normalized host too.
	c := browserGet("/page?x=1")
	c.host = "WIKI.Example.com.:443"
	resp, _ := e.checkGate(t, c)
	wantSignIn(t, "no cookie on "+c.host, resp, "https://wiki.example.com/page?x=1", "")
}

func TestGateCheckRefusesCookieFromAnotherHost(t *testing.T) {
	e := newTestEnv(t)
	docs := routeCookieFor(t, e, "dana", gateDocsHost)

	resp, _ := e.checkGate(t, browserGet("/page?x=1").with(docs))
	wantSignIn(t, "a browser with docs' cookie on wiki", resp, "https://wiki.example.com/page?x=1", "")

	resp, body := e.checkGate(t, gateCall{host: gateWikiHost, uri: "/api", method: http.MethodGet, accept: "application/json", cookie: docs})
	want401(t, "a script with docs' cookie on wiki", resp, body)

	// The cookie itself is good, on its own host.
	c := browserGet("/page").with(docs)
	c.host = gateDocsHost
	resp, _ = e.checkGate(t, c)
	wantPass(t, "docs' cookie on docs", resp, "dana")
}

func TestGateCheckRedirectsBrowsers(t *testing.T) {
	e := newTestEnv(t)
	for _, tc := range []struct {
		what string
		c    gateCall
	}{
		{"a GET", browserGet("/page?x=1")},
		{"Accept: text/html alone", gateCall{host: gateWikiHost, uri: "/page?x=1", method: http.MethodGet, accept: "text/html"}},
		{"a HEAD", gateCall{host: gateWikiHost, uri: "/page?x=1", method: http.MethodHead, accept: browserAccept}},
		{"an unknown cookie", browserGet("/page?x=1").with(NewToken())},
		{"a pending cookie already", browserGet("/page?x=1").withPending(testPending)},
	} {
		resp, _ := e.checkGate(t, tc.c)
		wantSignIn(t, tc.what, resp, "https://wiki.example.com/page?x=1", tc.c.pending)
	}
	if got, want := signInURL("https://wiki.example.com/page?x=1", nonceOf(testPending)),
		"https://slip.example.com/gate/start?n=a8ae6e6ee929abea3afcfc5258c8ccd6f85273e0d4626d26c7279f3250f77c8e"+
			"&rd=https%3A%2F%2Fwiki.example.com%2Fpage%3Fx%3D1"; got != want {
		t.Fatalf("the test's own sign-in URL is %q, want %q", got, want)
	}
}

func TestGateCheckBuildsHTTPSEvenBehindPlainProto(t *testing.T) {
	e := newTestEnv(t)
	c := browserGet("/page?x=1")
	// Only X-Forwarded-Host, -Uri and -Method count. The rest, including what a
	// tunnel's plain-HTTP hop says about the scheme, is ignored.
	c.extra = map[string]string{
		"X-Forwarded-Proto":  "http",
		"X-Forwarded-Port":   "80",
		"X-Forwarded-Prefix": "/evil",
		"Forwarded":          "proto=http;host=evil.example",
		"X-Real-Ip":          "203.0.113.9",
	}
	resp, _ := e.checkGate(t, c)
	wantSignIn(t, "plain proto", resp, "https://wiki.example.com/page?x=1", "")
}

func TestGateCheckGives401ToNonBrowsers(t *testing.T) {
	e := newTestEnv(t)
	for _, tc := range []struct {
		what string
		c    gateCall
	}{
		{"POST with Accept text/html", gateCall{host: gateWikiHost, uri: "/page?x=1", method: http.MethodPost, accept: browserAccept}},
		{"PUT with Accept text/html", gateCall{host: gateWikiHost, uri: "/page", method: http.MethodPut, accept: browserAccept}},
		{"GET with Accept application/json", gateCall{host: gateWikiHost, uri: "/page?x=1", method: http.MethodGet, accept: "application/json"}},
		{"GET with no Accept", gateCall{host: gateWikiHost, uri: "/ws", method: http.MethodGet}},
		{"GET with no forwarded method", gateCall{host: gateWikiHost, uri: "/page", accept: browserAccept}},
		{"GET with Upgrade: websocket", gateCall{host: gateWikiHost, uri: "/ws", method: http.MethodGet, accept: browserAccept,
			extra: map[string]string{"Upgrade": "websocket"}}},
		// A refusal neither sets nor refreshes a pending cookie.
		{"a script with a pending cookie", gateCall{host: gateWikiHost, uri: "/api", method: http.MethodGet, accept: "application/json", pending: testPending}},
	} {
		resp, body := e.checkGate(t, tc.c)
		want401(t, tc.what, resp, body)
	}
}

func TestGateCheckRedeemsCode(t *testing.T) {
	e := newTestEnv(t)
	code := mintGateCode(t, e, gateSignIn(t, e, "dana"), gateWikiHost, time.Minute)

	resp, _ := e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+code).withPending(testPending))
	route := wantRedeemed(t, "a good code", resp, "https://wiki.example.com/page?x=1")
	if resp.Header.Get(wireUserHeader) != "" {
		t.Errorf("the redirect carries %s", wireUserHeader)
	}
	ck := setCookie(resp, wireRouteCookie)
	if ck == nil {
		t.Fatal("no route cookie")
	}
	if ck.Path != "/" || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie Path=%q Secure=%v HttpOnly=%v SameSite=%v, want /, true, true, Lax", ck.Path, ck.Secure, ck.HttpOnly, ck.SameSite)
	}
	for _, raw := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(strings.ToLower(raw), "domain=") {
			t.Errorf("a cookie names a domain: %q", raw)
		}
	}
	if ck.Domain != "" {
		t.Errorf("the route cookie names a domain: %q", ck.Domain)
	}
	if ck.MaxAge != int((8 * time.Hour).Seconds()) {
		t.Errorf("Max-Age = %d, want %d", ck.MaxAge, int((8 * time.Hour).Seconds()))
	}
	if !isTokenHex(route) || route == code || route == testPending {
		t.Errorf("cookie value %q: want a new token", route)
	}

	// The cookie works on that host.
	resp, _ = e.checkGate(t, browserGet("/page?x=1").with(route))
	wantPass(t, "the new cookie", resp, "dana")

	// The code worked once.
	resp, _ = e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+code).withPending(testPending))
	wantSignIn(t, "the same code again", resp, "https://wiki.example.com/page?x=1", testPending)

	// A code alone leaves no empty query behind, and a HEAD redeems as well.
	code = mintGateCode(t, e, gateSignIn(t, e, "erin"), gateWikiHost, time.Minute)
	c := browserGet("/page?" + wireCodeParam + "=" + code).withPending(testPending)
	c.method = http.MethodHead
	resp, _ = e.checkGate(t, c)
	wantRedeemed(t, "a code alone, on a HEAD", resp, "https://wiki.example.com/page")
}

// A code works only in the browser that asked for it: the one holding the
// pending cookie whose hash the code was minted with. Anywhere else it is a
// bad code, and it stays unspent for the right browser.
func TestGateCheckCodeWorksOnlyInTheBrowserThatAskedForIt(t *testing.T) {
	e := newTestEnv(t)
	// The browser is sent to sign in, and gets its pending cookie.
	resp, _ := e.checkGate(t, browserGet("/page?x=1"))
	mine := wantSignIn(t, "first visit", resp, "https://wiki.example.com/page?x=1", "")
	// Sign-in mints a code for the n in that URL, as /gate/start will.
	n, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := mintGateCodeFor(t, e, gateSignIn(t, e, "dana"), gateWikiHost, n.Query().Get("n"), time.Minute)
	uri := "/page?x=1&" + wireCodeParam + "=" + code
	someoneElse := routeCookieFor(t, e, "erin", gateWikiHost)

	resp, _ = e.checkGate(t, browserGet(uri))
	wantSignIn(t, "no pending cookie", resp, "https://wiki.example.com/page?x=1", "")

	other := NewToken()
	resp, _ = e.checkGate(t, browserGet(uri).withPending(other))
	wantSignIn(t, "another browser's pending cookie", resp, "https://wiki.example.com/page?x=1", other)

	resp, _ = e.checkGate(t, browserGet(uri).withPending(other).with(someoneElse))
	wantRedirect(t, "another browser, signed in already", resp, "https://wiki.example.com/page?x=1")
	wantNoCookie(t, "another browser, signed in already", resp)

	resp, body := e.checkGate(t, gateCall{host: gateWikiHost, uri: uri, method: http.MethodGet, accept: "application/json", pending: other})
	want401(t, "a script with another pending cookie", resp, body)

	// n comes from a URL anyone can write. A code minted for the hash of
	// nothing must not work in a browser that has no pending cookie, or one
	// the gate couldn't have made.
	forNothing := mintGateCodeFor(t, e, gateSignIn(t, e, "mallory"), gateWikiHost, nonceOf(""), time.Minute)
	for _, p := range []string{"", "short"} {
		resp, _ = e.checkGate(t, browserGet("/x?"+wireCodeParam+"="+forNothing).withPending(p))
		wantSignIn(t, "a code for the hash of nothing, pending "+p, resp, "https://wiki.example.com/x", p)
	}

	// None of that spent the code.
	resp, _ = e.checkGate(t, browserGet(uri).withPending(mine))
	route := wantRedeemed(t, "the browser that asked", resp, "https://wiki.example.com/page?x=1")
	resp, _ = e.checkGate(t, browserGet("/page").with(route))
	wantPass(t, "its new cookie", resp, "dana")

	resp, _ = e.checkGate(t, browserGet(uri).withPending(mine))
	wantSignIn(t, "the same browser, the same code again", resp, "https://wiki.example.com/page?x=1", mine)
}

// The redirect to sign in gives the browser a pending cookie on the app's
// host and names its hash in the sign-in URL.
func TestGateCheckSignInSetsAPendingCookie(t *testing.T) {
	e := newTestEnv(t)
	resp, _ := e.checkGate(t, browserGet("/page?x=1"))
	first := wantSignIn(t, "first visit", resp, "https://wiki.example.com/page?x=1", "")
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query(); len(q) != 2 || q.Get("n") != nonceOf(first) || q.Get("rd") != "https://wiki.example.com/page?x=1" {
		t.Errorf("sign-in URL query %v, want exactly n = the pending cookie's hash and rd", q)
	}

	// Another browser gets a cookie of its own.
	resp, _ = e.checkGate(t, browserGet("/page?x=1"))
	if second := wantSignIn(t, "another browser", resp, "https://wiki.example.com/page?x=1", ""); second == first {
		t.Errorf("two browsers got the same pending cookie %q", first)
	}
}

// A browser that already has a pending cookie keeps it, so two tabs signing
// in to one app at once don't spoil each other's codes. Anything that isn't
// a token the gate could have made is replaced.
func TestGateCheckReusesThePendingCookie(t *testing.T) {
	e := newTestEnv(t)
	resp, _ := e.checkGate(t, browserGet("/a").withPending(testPending))
	wantSignIn(t, "a tab with a pending cookie", resp, "https://wiki.example.com/a", testPending)
	resp, _ = e.checkGate(t, browserGet("/b").withPending(testPending))
	wantSignIn(t, "a second tab", resp, "https://wiki.example.com/b", testPending)

	for _, bad := range []string{
		"short",
		strings.ToUpper(testPending),
		testPending + "0",
		testPending[:63] + "g",
	} {
		resp, _ := e.checkGate(t, browserGet("/a").withPending(bad))
		wantSignIn(t, "pending cookie "+bad, resp, "https://wiki.example.com/a", bad)
	}
}

// With no public URL, every browser visit to a protected route fails. It is
// logged, but not once per visit.
func TestGateCheckLogsMissingPublicURLOnceAMinute(t *testing.T) {
	e := newTestEnv(t)
	e.srv.publicURL = ""

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	for range 3 {
		resp, _ := e.checkGate(t, browserGet("/page"))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", resp.StatusCode)
		}
	}
	if n := strings.Count(logged.String(), "no public URL"); n != 1 {
		t.Errorf("three visits logged %d lines, want 1: %q", n, logged.String())
	}

	// A minute on, it is logged again.
	e.srv.gateNoPublicURLLog.mu.Lock()
	e.srv.gateNoPublicURLLog.last = time.Now().Add(-61 * time.Second)
	e.srv.gateNoPublicURLLog.mu.Unlock()
	e.checkGate(t, browserGet("/page"))
	if n := strings.Count(logged.String(), "no public URL"); n != 2 {
		t.Errorf("a visit a minute later: %d lines in all, want 2: %q", n, logged.String())
	}
}

func TestGateCheckBadCodeRestartsSignIn(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	used := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	if resp, _ := e.checkGate(t, browserGet("/?"+wireCodeParam+"="+used).withPending(testPending)); setCookie(resp, wireRouteCookie) == nil {
		t.Fatalf("setup: the first use of a code set no route cookie")
	}
	expired := mintGateCode(t, e, g, gateWikiHost, -time.Second)
	otherHost := mintGateCode(t, e, g, gateDocsHost, time.Minute)
	valid := routeCookieFor(t, e, "erin", gateWikiHost)

	for _, tc := range []struct{ what, code string }{
		{"reused", used},
		{"expired", expired},
		{"for another host", otherHost},
		{"unknown", NewToken()},
		{"empty", ""},
	} {
		// The browser holds the pending cookie these codes were minted for, so
		// each is refused for its own reason.
		uri := "/page?x=1&" + wireCodeParam + "=" + tc.code
		resp, _ := e.checkGate(t, browserGet(uri).withPending(testPending))
		wantSignIn(t, tc.what+", a browser", resp, "https://wiki.example.com/page?x=1", testPending)

		resp, body := e.checkGate(t, gateCall{host: gateWikiHost, uri: uri, method: http.MethodGet, accept: "application/json", pending: testPending})
		want401(t, tc.what+", a script", resp, body)

		resp, _ = e.checkGate(t, browserGet(uri).with(valid).withPending(testPending))
		wantRedirect(t, tc.what+", with a valid cookie", resp, "https://wiki.example.com/page?x=1")
		wantNoCookie(t, tc.what+", with a valid cookie", resp)
	}

	// A code for another host is kept, and still works there.
	c := browserGet("/?" + wireCodeParam + "=" + otherHost).withPending(testPending)
	c.host = gateDocsHost
	resp, _ := e.checkGate(t, c)
	wantRedeemed(t, "the docs code on docs", resp, "https://docs.example.com/")

	// A code is redeemed only for a GET or HEAD. One on a POST is refused and
	// stays unspent.
	code := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	post := gateCall{host: gateWikiHost, uri: "/page?" + wireCodeParam + "=" + code, method: http.MethodPost, accept: browserAccept, pending: testPending}
	resp, _ = e.checkGate(t, post)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a code on a POST: %d, want 400", resp.StatusCode)
	}
	wantNoCookie(t, "a code on a POST", resp)
	resp, _ = e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+code).withPending(testPending))
	wantRedeemed(t, "the POST's code, then on a GET", resp, "https://wiki.example.com/page")
}

func TestGateCheckFailsClosedWithoutPublicURL(t *testing.T) {
	e := newTestEnv(t)
	tok := routeCookieFor(t, e, "dana", gateWikiHost)
	e.srv.publicURL = ""

	resp, _ := e.checkGate(t, browserGet("/page?x=1"))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Location") != "" {
		t.Errorf("no cookie: %d to %q, want 503 and no Location", resp.StatusCode, resp.Header.Get("Location"))
	}
	wantNoCookie(t, "no cookie, no public URL", resp)
	resp, _ = e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+NewToken()).withPending(testPending))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Location") != "" {
		t.Errorf("a bad code: %d to %q, want 503 and no Location", resp.StatusCode, resp.Header.Get("Location"))
	}
	wantNoCookie(t, "a bad code, no public URL", resp)
	resp, body := e.checkGate(t, gateCall{host: gateWikiHost, uri: "/api", method: http.MethodGet, accept: "application/json"})
	want401(t, "a script", resp, body)

	resp, _ = e.checkGate(t, browserGet("/page?x=1").with(tok))
	wantPass(t, "a valid cookie", resp, "dana")
}

func TestGateCheckNeverRedirectsOffTheHost(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	valid := routeCookieFor(t, e, "erin", gateWikiHost)

	for _, tc := range []struct{ uri, want string }{
		{"//evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"/\\evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"///evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"\\\\evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"/\\/\\evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/evil.example/"},
		{"@evil.example/?" + wireCodeParam + "=%s", "https://wiki.example.com/@evil.example/"},
		{"?" + wireCodeParam + "=%s", "https://wiki.example.com/"},
		{"/a b\t/c?" + wireCodeParam + "=%s", "https://wiki.example.com/a%20b%09/c"},
		// A request target has no fragment, so a "#" in one is data. Left
		// alone it would start one, and a code appended later would land in it.
		{"/p#x?a=1#y&" + wireCodeParam + "=%s", "https://wiki.example.com/p%23x?a=1%23y"},
	} {
		good := mintGateCode(t, e, g, gateWikiHost, time.Minute)
		resp, _ := e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", good, 1)).withPending(testPending))
		wantRedeemed(t, tc.uri+", a good code", resp, tc.want)

		resp, _ = e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", NewToken(), 1)).with(valid).withPending(testPending))
		wantRedirect(t, tc.uri+", a bad code and a valid cookie", resp, tc.want)

		resp, _ = e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", NewToken(), 1)).withPending(testPending))
		wantSignIn(t, tc.uri+", a bad code and no cookie", resp, tc.want, testPending)
	}

	// The parameter given twice is removed both times.
	good := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	resp, _ := e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+good+"&x=1&"+wireCodeParam+"="+NewToken()).withPending(testPending))
	wantRedeemed(t, "two codes", resp, "https://wiki.example.com/page?x=1")
}

// Only the first code is tried, every one is removed, and the rest of the
// query keeps its order and spelling. An escaped spelling of the name counts.
func TestGateCheckTriesTheFirstCodeAndStripsThemAll(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	second := mintGateCode(t, e, g, gateWikiHost, time.Minute)

	resp, _ := e.checkGate(t, browserGet("/p?b=2&"+wireCodeParam+"="+NewToken()+"&a=%20x+y&"+wireCodeParam+"="+second+"&c").withPending(testPending))
	wantSignIn(t, "a bad first code", resp, "https://wiki.example.com/p?b=2&a=%20x+y&c", testPending)

	// The second code wasn't tried, so it still works.
	resp, _ = e.checkGate(t, browserGet("/p?__outhaul%5Fcode="+second+"&z=1").withPending(testPending))
	wantRedeemed(t, "the untried code, its name escaped", resp, "https://wiki.example.com/p?z=1")
}

func TestGateCheckRepliesArentCached(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	valid := routeCookieFor(t, e, "erin", gateWikiHost)
	calls := map[string]gateCall{
		"a pass":                        browserGet("/").with(valid),
		"sign in":                       browserGet("/page"),
		"a good code":                   browserGet("/page?" + wireCodeParam + "=" + mintGateCode(t, e, g, gateWikiHost, time.Minute)).withPending(testPending),
		"a bad code with a cookie":      browserGet("/page?" + wireCodeParam + "=nope").with(valid),
		"a bad code without a cookie":   browserGet("/page?" + wireCodeParam + "=nope"),
		"a script":                      {host: gateWikiHost, uri: "/api", method: http.MethodGet, accept: "application/json"},
		"no host":                       {uri: "/page", method: http.MethodGet, accept: browserAccept},
		"a script with a bad code":      {host: gateWikiHost, uri: "/api?" + wireCodeParam + "=nope", method: http.MethodGet},
		"a bad host":                    {host: "evil.example/x", uri: "/", method: http.MethodGet, accept: browserAccept},
		"a POST with a cookie and code": {host: gateWikiHost, uri: "/?" + wireCodeParam + "=nope", method: http.MethodPost, cookie: valid},
	}
	for what, c := range calls {
		resp, _ := e.checkGate(t, c)
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s (%d): Cache-Control = %q, want no-store", what, resp.StatusCode, cc)
		}
	}
	e.srv.publicURL = ""
	resp, _ := e.checkGate(t, browserGet("/page"))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("no public URL: %d with Cache-Control %q, want 503 with no-store", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

// A host that can't be a domain row is refused outright, before any code is
// spent or cookie read.
func TestGateCheckRefusesImplausibleHosts(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	h := e.srv.Handler()
	for _, host := range []string{
		"", "evil.example/x", "dana@wiki.example.com", "wiki example.com", "wiki.example.com\\evil",
		"wiki.example.com\x00", "wiki.example.com\x7f", ":443", "[::1]:443", "wiki.example.com?x", "wiki.example.com#x",
		"wiki.example.com, evil.example",
		"wiKi.example.com", // a Kelvin sign, which lower-cases to "k"
	} {
		code := mintGateCode(t, e, g, host, time.Minute)
		req := httptest.NewRequest(http.MethodGet, "/gate/check", nil)
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Forwarded-Uri", "/page?"+wireCodeParam+"="+code)
		req.Header.Set("X-Forwarded-Method", http.MethodGet)
		req.Header.Set("Accept", browserAccept)
		req.AddCookie(&http.Cookie{Name: wirePendingCookie, Value: testPending})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" || len(rec.Header().Values("Set-Cookie")) > 0 {
			t.Errorf("host %q: %d to %q, Set-Cookie %q; want a bare 400", host, rec.Code, rec.Header().Get("Location"), rec.Header().Values("Set-Cookie"))
		}
	}

	// No X-Forwarded-Host at all.
	resp, _ := e.checkGate(t, gateCall{uri: "/page", method: http.MethodGet, accept: browserAccept})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no X-Forwarded-Host: %d, want 400", resp.StatusCode)
	}
}

// The gate is a GET. Nothing else reaches it.
func TestGateCheckAnswersOnlyGET(t *testing.T) {
	e := newTestEnv(t)
	valid := routeCookieFor(t, e, "dana", gateWikiHost)
	for _, m := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(m, e.http.URL+"/gate/check", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-Host", gateWikiHost)
		req.Header.Set("X-Forwarded-Uri", "/")
		req.Header.Set("X-Forwarded-Method", http.MethodGet)
		req.AddCookie(&http.Cookie{Name: wireRouteCookie, Value: valid})
		resp, err := accountClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get(wireUserHeader) != "" {
			t.Errorf("%s /gate/check = %d with %s %q, want 405 and no user", m, resp.StatusCode, wireUserHeader, resp.Header.Get(wireUserHeader))
		}
	}
}

// A store that fails is neither a pass nor a reason to sign in again: a
// redirect would send everyone round the sign-in loop until it recovered.
func TestGateCheckAnswers503WhenTheStoreFails(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	valid := routeCookieFor(t, e, "erin", gateWikiHost)
	code := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	db := e.rawDB(t)
	if _, err := db.Exec(`UPDATE route_sessions SET expires_at = 'not a time'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE gate_codes SET expires_at = 'not a time'`); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	for what, c := range map[string]gateCall{
		"a cookie":                      browserGet("/page?x=1").with(valid),
		"a code":                        browserGet("/page?x=1&" + wireCodeParam + "=" + code).withPending(testPending),
		"a bad code and a cookie":       browserGet("/page?x=1&" + wireCodeParam + "=" + NewToken()).with(valid).withPending(testPending),
		"a script's call with a cookie": {host: gateWikiHost, uri: "/api", method: http.MethodPost, accept: "application/json", cookie: valid},
	} {
		resp, body := e.checkGate(t, c)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d (Location %q), want 503", what, resp.StatusCode, resp.Header.Get("Location"))
		}
		if resp.Header.Get("Location") != "" || resp.Header.Get(wireUserHeader) != "" {
			t.Errorf("%s: Location %q, user %q; want neither", what, resp.Header.Get("Location"), resp.Header.Get(wireUserHeader))
		}
		wantNoCookie(t, what, resp)
		if strings.Contains(body, "not a time") {
			t.Errorf("%s: the store's error reached the browser: %q", what, body)
		}
	}
	out := logged.String()
	if !strings.Contains(out, gateWikiHost) || !strings.Contains(out, "not a time") {
		t.Errorf("the failures weren't logged with the host and cause: %q", out)
	}
	for _, secret := range []string{valid, code, "x=1"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q: %q", secret, out)
		}
	}
}

// Codes, cookies and the query (which can hold a code) never reach the log.
func TestGateCheckLogsNoSecrets(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	valid := routeCookieFor(t, e, "erin", gateWikiHost)
	good := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	bad := NewToken()
	other := mintGateCode(t, e, g, gateDocsHost, time.Minute)

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	var issued []string
	for _, c := range []gateCall{
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + good).withPending(testPending),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + bad).withPending(testPending),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + other).withPending(testPending),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + good),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + bad).with(valid),
		browserGet("/secret-path?q=private").with(valid),
		browserGet("/secret-path?q=private"),
	} {
		resp, _ := e.checkGate(t, c)
		for _, ck := range resp.Cookies() {
			if ck.Value != "" {
				issued = append(issued, ck.Value)
			}
		}
	}
	out := logged.String()
	if !strings.Contains(out, gateWikiHost) {
		t.Errorf("a refused code wasn't logged with its host: %q", out)
	}
	for _, secret := range append([]string{good, bad, other, valid, testPending, nonceOf(testPending), "private", "secret-path"}, issued...) {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q: %q", secret, out)
		}
	}
}
func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"wiki.example.com":      "wiki.example.com",
		"WIKI.Example.COM":      "wiki.example.com",
		"wiki.example.com:443":  "wiki.example.com",
		"wiki.example.com:8443": "wiki.example.com",
		"wiki.example.com.":     "wiki.example.com",
		"WIKI.example.com.:443": "wiki.example.com",
		"wiki.example.com..":    "wiki.example.com.",
		"":                      "",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGateStartURL(t *testing.T) {
	e := newTestEnv(t)
	rd := "https://wiki.example.com/a b?x=1&y=2#frag"
	n := nonceOf(testPending)
	want := "https://slip.example.com/gate/start?n=" + n + "&rd=" + url.QueryEscape(rd)
	for _, pub := range []string{"https://slip.example.com", "https://slip.example.com/"} {
		e.srv.publicURL = pub
		got := e.srv.gateStartURL(rd, n)
		if got != want {
			t.Errorf("public URL %q: %q, want %q", pub, got, want)
		}
		u, err := url.Parse(got)
		if err != nil || u.Query().Get("rd") != rd || u.Query().Get("n") != n || len(u.Query()) != 2 {
			t.Errorf("public URL %q: %q doesn't carry rd and n back intact", pub, got)
		}
	}
	if got, want := e.srv.gateStartURL(rd, ""), "https://slip.example.com/gate/start?rd="+url.QueryEscape(rd); got != want {
		t.Errorf("no nonce: %q, want %q", got, want)
	}
	e.srv.publicURL = ""
	if got := e.srv.gateStartURL(rd, n); got != "" {
		t.Errorf("no public URL: %q, want \"\"", got)
	}
}
