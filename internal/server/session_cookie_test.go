package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// loginOn signs in with a password on this Host header, as a proxy in front
// that sets X-Forwarded-Proto to proto ("" for none) would send it.
func loginOn(t *testing.T, e *testEnv, host, proto string) *http.Response {
	t.Helper()
	form := url.Values{"username": {"admin"}, "password": {"supersecret"}}
	req, err := http.NewRequest(http.MethodPost, e.http.URL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if host != "" {
		req.Host = host
	}
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	resp, err := accountClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// The session cookie is Secure on an HTTPS public URL's own host whatever the
// request looks like: in tunnel mode Traefik serves plain HTTP and says so in
// X-Forwarded-Proto while browsers are on HTTPS. Elsewhere, on a LAN address
// or through an SSH tunnel, it is Secure only if the request was HTTPS, so a
// password sign-in over plain HTTP still works.
func TestSessionCookieFollowsThePublicURL(t *testing.T) {
	e := newTestEnv(t)
	passwordAdmin(t, e, "admin", "supersecret")
	for _, c := range []struct {
		public, host, proto string
		secure              bool
	}{
		{"https://outhaul.example.com", "outhaul.example.com", "", true},
		{"https://outhaul.example.com", "outhaul.example.com", "http", true}, // tunnel mode
		{"https://outhaul.example.com", "OUTHAUL.Example.com:443", "", true},
		{"https://outhaul.example.com/", "outhaul.example.com", "https", true},
		{"https://outhaul.example.com", "10.0.0.5:8080", "", false},  // LAN
		{"https://outhaul.example.com", "localhost:8080", "", false}, // SSH tunnel
		{"https://outhaul.example.com", "outhaul.example.com:8080", "", false},
		{"https://outhaul.example.com", "10.0.0.5:8080", "https", true},
		{"http://outhaul.example.com", "outhaul.example.com", "", false},
		{"http://outhaul.example.com", "outhaul.example.com", "https", true},
		{"", "outhaul.example.com", "", false},
	} {
		e.srv.publicURL = c.public
		resp := loginOn(t, e, c.host, c.proto)
		sc, ok := sessionCookieIn(resp)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || !ok {
			t.Errorf("public URL %q, host %q, proto %q: sign-in = %d to %q, cookie %v; want 303 to / with a session",
				c.public, c.host, c.proto, resp.StatusCode, resp.Header.Get("Location"), ok)
			continue
		}
		if sc.Secure != c.secure || sc.Name != sessionCookie || sc.Path != "/" || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
			t.Errorf("public URL %q, host %q, proto %q: session cookie = %+v, want Secure %v", c.public, c.host, c.proto, sc, c.secure)
		}
		if !hasSession(e, sc.Value) {
			t.Errorf("public URL %q, host %q, proto %q: the session doesn't work", c.public, c.host, c.proto)
		}
	}
}

// Signing out clears the cookie with the same Secure flag it was set with.
func TestSignOutClearsTheCookieLikeItWasSet(t *testing.T) {
	e := newTestEnv(t)
	e.srv.publicURL = "https://outhaul.example.com"
	for _, c := range []struct {
		host, proto string
		secure      bool
	}{
		{"outhaul.example.com", "http", true},
		{"10.0.0.5:8080", "", false},
	} {
		token := e.signInAs(t, "pat-"+strings.NewReplacer(".", "-", ":", "-").Replace(c.host), core.RoleProjectViewer)
		req, err := http.NewRequest(http.MethodPost, e.http.URL+"/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		resp, err := accountClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		sc, ok := sessionCookieIn(resp)
		if !ok || sc.MaxAge >= 0 || sc.Secure != c.secure || sc.Path != "/" || !sc.HttpOnly {
			t.Errorf("host %q, proto %q: cleared cookie = %+v, want MaxAge<0, Secure %v", c.host, c.proto, sc, c.secure)
		}
		if hasSession(e, token) {
			t.Errorf("host %q: the session still works after signing out", c.host)
		}
	}
}
