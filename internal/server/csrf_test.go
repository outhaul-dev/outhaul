package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postProject builds an authenticated create-project POST carrying the given
// browser fetch-metadata headers.
func (e *testEnv) postProject(name string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(url.Values{"name": {name}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	e.authed(req)
	return req
}

func (e *testEnv) projectExists(t *testing.T, name string) bool {
	t.Helper()
	ps, err := e.store.ListProjects(t.Context())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// A sibling subdomain (a PR preview, any app on the same registrable domain)
// is same-site, so SameSite=Lax still attaches the session cookie. The
// request must be refused on its fetch metadata, not on the cookie.
func TestSameSitePostIsRejected(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, e.postProject("evil", map[string]string{"Sec-Fetch-Site": "same-site"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("same-site POST status = %d, want 403", rec.Code)
	}
	if e.projectExists(t, "evil") {
		t.Fatal("same-site POST created a project")
	}
}

func TestCrossOriginPostWithoutFetchMetadataIsRejected(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	req := e.postProject("evil", map[string]string{"Origin": "https://pr-7.preview.example.com"})
	req.Host = "slip.example.com"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
}

func TestSameOriginPostIsAllowed(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)

	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, e.postProject("shop", map[string]string{"Sec-Fetch-Site": "same-origin"}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin POST status = %d, want 303", rec.Code)
	}
	if !e.projectExists(t, "shop") {
		t.Fatal("same-origin POST did not create the project")
	}
}

// Git hosts deliver webhooks server-to-server, with no browser fetch metadata
// and no Origin; cross-origin protection must not get in their way.
func TestWebhookIsNotBlockedByCrossOriginProtection(t *testing.T) {
	e := newTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/app/nope", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("webhook POST was rejected as cross-origin (403)")
	}
}

func TestSessionCookieIsSecureOverHTTPS(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   bool
	}{
		{"plain http", func(*http.Request) {}, false},
		{"behind TLS-terminating proxy", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			form := url.Values{"token": {"SETUPTOKEN"}, "username": {"admin"}, "password": {"supersecret"}}
			req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			tc.mutate(req)
			rec := httptest.NewRecorder()
			e.srv.Handler().ServeHTTP(rec, req)

			var got *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie {
					got = c
				}
			}
			if got == nil {
				t.Fatalf("no session cookie set (status %d)", rec.Code)
			}
			if got.Secure != tc.want {
				t.Fatalf("session cookie Secure = %v, want %v", got.Secure, tc.want)
			}
		})
	}
}
