package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestPlaceholderPageRenders(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/databases")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /databases = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "Databases") || !strings.Contains(page, "coming soon") {
		t.Error("placeholder page should show its title and a coming-soon marker")
	}
}

func TestPlaceholderRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/volumes")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /volumes = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}
