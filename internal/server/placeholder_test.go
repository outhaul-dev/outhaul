package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestPlaceholderPageRenders(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	resp := e.get(t, "/registry")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /registry = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "Registry") || !strings.Contains(page, "coming soon") {
		t.Error("placeholder page should show its title and a coming-soon marker")
	}
}

func TestPlaceholderRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	resp := e.get(t, "/registry")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth /registry = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}
