package server

import (
	"bytes"
	"context"
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
	gateWikiHost    = "wiki.example.com"
	gateDocsHost    = "docs.example.com"
	browserAccept   = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	wireCodeParam   = "__outhaul_code"
	wireRouteCookie = "__Host-outhaul_route"
	wireUserHeader  = "X-Outhaul-User"
)

// gateCall is one visitor request as Traefik's forward auth describes it to
// the gate: the forwarded headers it sets, plus the browser's own headers,
// which it copies.
type gateCall struct {
	host, uri, method, accept string
	cookie                    string            // the route cookie's value; "" sends none
	extra                     map[string]string // any other header; set last, so it can override
}

// browserGet is a page navigation to uri on wiki.example.com.
func browserGet(uri string) gateCall {
	return gateCall{host: gateWikiHost, uri: uri, method: http.MethodGet, accept: browserAccept}
}

func (c gateCall) with(cookie string) gateCall { c.cookie = cookie; return c }

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

// mintGateCode stores a one-time code that carries gateTok to host, expiring
// after ttl.
func mintGateCode(t *testing.T, e *testEnv, gateTok, host string, ttl time.Duration) string {
	t.Helper()
	code := NewToken()
	if err := e.store.CreateGateCode(context.Background(), code, gateTok, host, time.Now().Add(ttl)); err != nil {
		t.Fatalf("CreateGateCode: %v", err)
	}
	return code
}

// routeCookieFor signs display in to host and returns the route cookie's value.
func routeCookieFor(t *testing.T, e *testEnv, display, host string) string {
	t.Helper()
	code := mintGateCode(t, e, gateSignIn(t, e, display), host, time.Minute)
	tok := NewToken()
	if _, err := e.store.RedeemGateCode(context.Background(), code, host, tok, time.Now().Add(8*time.Hour)); err != nil {
		t.Fatalf("RedeemGateCode: %v", err)
	}
	return tok
}

// signInURL is where a browser bound for returnURL is sent to sign in.
func signInURL(returnURL string) string {
	return "https://slip.example.com/gate/start?rd=" + url.QueryEscape(returnURL)
}

// wantSignIn checks the answer sends the browser to sign in, to come back to
// returnURL, and sets no cookie.
func wantSignIn(t *testing.T, what string, resp *http.Response, returnURL string) {
	t.Helper()
	if want := signInURL(returnURL); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
		t.Errorf("%s: %d to %q, want 302 to %q", what, resp.StatusCode, resp.Header.Get("Location"), want)
	}
	wantNoCookie(t, what, resp)
	if u := resp.Header.Get(wireUserHeader); u != "" {
		t.Errorf("%s: a redirect carries %s %q", what, wireUserHeader, u)
	}
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
	wantSignIn(t, "no cookie on "+c.host, resp, "https://wiki.example.com/page?x=1")
}

func TestGateCheckRefusesCookieFromAnotherHost(t *testing.T) {
	e := newTestEnv(t)
	docs := routeCookieFor(t, e, "dana", gateDocsHost)

	resp, _ := e.checkGate(t, browserGet("/page?x=1").with(docs))
	wantSignIn(t, "a browser with docs' cookie on wiki", resp, "https://wiki.example.com/page?x=1")

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
	} {
		resp, _ := e.checkGate(t, tc.c)
		wantSignIn(t, tc.what, resp, "https://wiki.example.com/page?x=1")
	}
	if got, want := signInURL("https://wiki.example.com/page?x=1"),
		"https://slip.example.com/gate/start?rd=https%3A%2F%2Fwiki.example.com%2Fpage%3Fx%3D1"; got != want {
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
	wantSignIn(t, "plain proto", resp, "https://wiki.example.com/page?x=1")
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
	} {
		resp, body := e.checkGate(t, tc.c)
		want401(t, tc.what, resp, body)
	}
}

func TestGateCheckRedeemsCode(t *testing.T) {
	e := newTestEnv(t)
	code := mintGateCode(t, e, gateSignIn(t, e, "dana"), gateWikiHost, time.Minute)

	resp, _ := e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+code))
	wantRedirect(t, "a good code", resp, "https://wiki.example.com/page?x=1")
	if resp.Header.Get(wireUserHeader) != "" {
		t.Errorf("the redirect carries %s", wireUserHeader)
	}
	raw := resp.Header.Values("Set-Cookie")
	cookies := resp.Cookies()
	if len(raw) != 1 || len(cookies) != 1 {
		t.Fatalf("Set-Cookie = %q, want exactly one cookie", raw)
	}
	ck := cookies[0]
	if ck.Name != wireRouteCookie {
		t.Errorf("cookie name = %q, want %q", ck.Name, wireRouteCookie)
	}
	if ck.Path != "/" || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie Path=%q Secure=%v HttpOnly=%v SameSite=%v, want /, true, true, Lax", ck.Path, ck.Secure, ck.HttpOnly, ck.SameSite)
	}
	if ck.Domain != "" || strings.Contains(strings.ToLower(raw[0]), "domain=") {
		t.Errorf("the cookie names a domain: %q", raw[0])
	}
	if ck.MaxAge != int((8 * time.Hour).Seconds()) {
		t.Errorf("Max-Age = %d, want %d", ck.MaxAge, int((8 * time.Hour).Seconds()))
	}
	if ck.Value == "" || ck.Value == code {
		t.Errorf("cookie value %q: want a new token, not the code", ck.Value)
	}

	// The cookie works on that host.
	resp, _ = e.checkGate(t, browserGet("/page?x=1").with(ck.Value))
	wantPass(t, "the new cookie", resp, "dana")

	// The code worked once.
	resp, _ = e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+code))
	wantSignIn(t, "the same code again", resp, "https://wiki.example.com/page?x=1")

	// A code alone leaves no empty query behind, and a HEAD redeems as well.
	code = mintGateCode(t, e, gateSignIn(t, e, "erin"), gateWikiHost, time.Minute)
	c := browserGet("/page?" + wireCodeParam + "=" + code)
	c.method = http.MethodHead
	resp, _ = e.checkGate(t, c)
	wantRedirect(t, "a code alone, on a HEAD", resp, "https://wiki.example.com/page")
	if len(resp.Cookies()) != 1 {
		t.Errorf("a HEAD with a good code set %d cookies, want 1", len(resp.Cookies()))
	}
}

func TestGateCheckBadCodeRestartsSignIn(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	used := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	if resp, _ := e.checkGate(t, browserGet("/?"+wireCodeParam+"="+used)); len(resp.Cookies()) != 1 {
		t.Fatalf("setup: the first use of a code set %d cookies", len(resp.Cookies()))
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
		uri := "/page?x=1&" + wireCodeParam + "=" + tc.code
		resp, _ := e.checkGate(t, browserGet(uri))
		wantSignIn(t, tc.what+", a browser", resp, "https://wiki.example.com/page?x=1")

		resp, body := e.checkGate(t, gateCall{host: gateWikiHost, uri: uri, method: http.MethodGet, accept: "application/json"})
		want401(t, tc.what+", a script", resp, body)

		resp, _ = e.checkGate(t, browserGet(uri).with(valid))
		wantRedirect(t, tc.what+", with a valid cookie", resp, "https://wiki.example.com/page?x=1")
		wantNoCookie(t, tc.what+", with a valid cookie", resp)
	}

	// A code for another host is kept, and still works there.
	c := browserGet("/?" + wireCodeParam + "=" + otherHost)
	c.host = gateDocsHost
	resp, _ := e.checkGate(t, c)
	wantRedirect(t, "the docs code on docs", resp, "https://docs.example.com/")
	if len(resp.Cookies()) != 1 {
		t.Errorf("the docs code on docs set %d cookies, want 1", len(resp.Cookies()))
	}

	// A code is redeemed only for a GET or HEAD. One on a POST stays unspent.
	code := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	post := gateCall{host: gateWikiHost, uri: "/page?" + wireCodeParam + "=" + code, method: http.MethodPost, accept: browserAccept}
	resp, body := e.checkGate(t, post)
	want401(t, "a code on a POST", resp, body)
	resp, _ = e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+code))
	wantRedirect(t, "the POST's code, then on a GET", resp, "https://wiki.example.com/page")
	if len(resp.Cookies()) != 1 {
		t.Errorf("the code was spent by a POST")
	}
}

func TestGateCheckFailsClosedWithoutPublicURL(t *testing.T) {
	e := newTestEnv(t)
	tok := routeCookieFor(t, e, "dana", gateWikiHost)
	e.srv.publicURL = ""

	resp, _ := e.checkGate(t, browserGet("/page?x=1"))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Location") != "" {
		t.Errorf("no cookie: %d to %q, want 503 and no Location", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.checkGate(t, browserGet("/page?x=1&"+wireCodeParam+"="+NewToken()))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Location") != "" {
		t.Errorf("a bad code: %d to %q, want 503 and no Location", resp.StatusCode, resp.Header.Get("Location"))
	}
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
		resp, _ := e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", good, 1)))
		wantRedirect(t, tc.uri+", a good code", resp, tc.want)
		if len(resp.Cookies()) != 1 {
			t.Errorf("%s, a good code: set %d cookies, want 1", tc.uri, len(resp.Cookies()))
		}

		resp, _ = e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", NewToken(), 1)).with(valid))
		wantRedirect(t, tc.uri+", a bad code and a valid cookie", resp, tc.want)

		resp, _ = e.checkGate(t, browserGet(strings.Replace(tc.uri, "%s", NewToken(), 1)))
		wantSignIn(t, tc.uri+", a bad code and no cookie", resp, tc.want)
	}

	// The parameter given twice is removed both times.
	good := mintGateCode(t, e, g, gateWikiHost, time.Minute)
	resp, _ := e.checkGate(t, browserGet("/page?"+wireCodeParam+"="+good+"&x=1&"+wireCodeParam+"="+NewToken()))
	wantRedirect(t, "two codes", resp, "https://wiki.example.com/page?x=1")
}

// Only the first code is tried, every one is removed, and the rest of the
// query keeps its order and spelling. An escaped spelling of the name counts.
func TestGateCheckTriesTheFirstCodeAndStripsThemAll(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	second := mintGateCode(t, e, g, gateWikiHost, time.Minute)

	resp, _ := e.checkGate(t, browserGet("/p?b=2&"+wireCodeParam+"="+NewToken()+"&a=%20x+y&"+wireCodeParam+"="+second+"&c"))
	wantSignIn(t, "a bad first code", resp, "https://wiki.example.com/p?b=2&a=%20x+y&c")

	// The second code wasn't tried, so it still works.
	resp, _ = e.checkGate(t, browserGet("/p?__outhaul%5Fcode="+second+"&z=1"))
	wantRedirect(t, "the untried code, its name escaped", resp, "https://wiki.example.com/p?z=1")
	if len(resp.Cookies()) != 1 {
		t.Errorf("the untried code set %d cookies, want 1", len(resp.Cookies()))
	}
}

func TestGateCheckRepliesArentCached(t *testing.T) {
	e := newTestEnv(t)
	g := gateSignIn(t, e, "dana")
	valid := routeCookieFor(t, e, "erin", gateWikiHost)
	calls := map[string]gateCall{
		"a pass":                        browserGet("/").with(valid),
		"sign in":                       browserGet("/page"),
		"a good code":                   browserGet("/page?" + wireCodeParam + "=" + mintGateCode(t, e, g, gateWikiHost, time.Minute)),
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
		"a code":                        browserGet("/page?x=1&" + wireCodeParam + "=" + code),
		"a bad code and a cookie":       browserGet("/page?x=1&" + wireCodeParam + "=" + NewToken()).with(valid),
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
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + good),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + bad),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + other),
		browserGet("/secret-path?q=private&" + wireCodeParam + "=" + bad).with(valid),
		browserGet("/secret-path?q=private").with(valid),
		browserGet("/secret-path?q=private"),
	} {
		resp, _ := e.checkGate(t, c)
		for _, ck := range resp.Cookies() {
			issued = append(issued, ck.Value)
		}
	}
	out := logged.String()
	if !strings.Contains(out, gateWikiHost) {
		t.Errorf("a refused code wasn't logged with its host: %q", out)
	}
	for _, secret := range append([]string{good, bad, other, valid, "private", "secret-path"}, issued...) {
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
	want := "https://slip.example.com/gate/start?rd=" + url.QueryEscape(rd)
	for _, pub := range []string{"https://slip.example.com", "https://slip.example.com/"} {
		e.srv.publicURL = pub
		got := e.srv.gateStartURL(rd)
		if got != want {
			t.Errorf("public URL %q: %q, want %q", pub, got, want)
		}
		u, err := url.Parse(got)
		if err != nil || u.Query().Get("rd") != rd || len(u.Query()) != 1 {
			t.Errorf("public URL %q: %q doesn't carry rd back intact", pub, got)
		}
	}
	e.srv.publicURL = ""
	if got := e.srv.gateStartURL(rd); got != "" {
		t.Errorf("no public URL: %q, want \"\"", got)
	}
}
