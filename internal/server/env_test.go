package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestSetEnvWithScope(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})

	// prod-scoped var
	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{"key": {"SECRET_KEY"}, "value": {"x"}, "scope": {"prod"}})
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("set env status = %d, want 303", r.StatusCode)
	}
	vars, _ := e.store.ListEnv(context.Background(), app.ID)
	if got := scopeOf(vars, "SECRET_KEY"); got != core.ScopeProd {
		t.Errorf("SECRET_KEY scope = %q, want %q", got, core.ScopeProd)
	}

	// preview-scoped var
	r2 := e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{"key": {"PREVIEW_KEY"}, "value": {"y"}, "scope": {"preview"}})
	r2.Body.Close()
	if r2.StatusCode != http.StatusSeeOther {
		t.Fatalf("set preview env status = %d, want 303", r2.StatusCode)
	}
	vars, _ = e.store.ListEnv(context.Background(), app.ID)
	if got := scopeOf(vars, "PREVIEW_KEY"); got != core.ScopePreview {
		t.Errorf("PREVIEW_KEY scope = %q, want %q", got, core.ScopePreview)
	}

	// invalid scope rejected
	r3 := e.postForm(t, "/apps/"+itoa(app.ID)+"/env", url.Values{"key": {"BOGUS_KEY"}, "value": {"z"}, "scope": {"bogus"}})
	r3.Body.Close()
	if r3.StatusCode < 400 {
		t.Errorf("invalid scope should be rejected, got %d", r3.StatusCode)
	}
}

func scopeOf(vars []core.EnvVar, key string) string {
	for _, v := range vars {
		if v.Key == key {
			return v.Scope
		}
	}
	return "<missing>"
}
