package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
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

func TestDeleteEnvRemovesOnlyTheSubmittedScope(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, _ := e.store.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	_ = e.store.SetEnvScoped(context.Background(), app.ID, "DATABASE_URL", "p", true, core.ScopeProd)
	_ = e.store.SetEnvScoped(context.Background(), app.ID, "DATABASE_URL", "v", true, core.ScopePreview)

	r := e.postForm(t, "/apps/"+itoa(app.ID)+"/env/delete", url.Values{"key": {"DATABASE_URL"}, "scope": {"preview"}})
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete env status = %d, want 303", r.StatusCode)
	}
	vars, _ := e.store.ListEnv(context.Background(), app.ID)
	if len(vars) != 1 || vars[0].Scope != core.ScopeProd {
		t.Fatalf("after deleting the preview row: %+v", vars)
	}
}

// Once a key has a production- or preview-only value, its shared row no
// longer reaches every deploy. The page must say so: re-adding a secret as
// "Production only" leaves the shared copy flowing to (public) previews.
func TestAppPageFlagsOverriddenSharedEnv(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	ctx := context.Background()
	app, _ := e.store.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://x/y.git", Domain: "web.test"})
	_ = e.store.SetEnvScoped(ctx, app.ID, "DATABASE_URL", "shared", true, core.ScopeShared)
	_ = e.store.SetEnvScoped(ctx, app.ID, "DATABASE_URL", "prod", true, core.ScopeProd)
	_ = e.store.SetEnvScoped(ctx, app.ID, "LOG_LEVEL", "info", false, core.ScopeShared)

	page := body(t, e.get(t, "/apps/"+itoa(app.ID)))
	if !strings.Contains(page, "previews only") {
		t.Errorf("app page does not flag the overridden shared DATABASE_URL")
	}
	if strings.Count(page, "previews only") != 1 {
		t.Errorf("hint should appear once (only the overridden row), got %d", strings.Count(page, "previews only"))
	}
}

func TestSharedEnvReach(t *testing.T) {
	vars := []core.EnvVar{
		{Key: "A", Scope: core.ScopeShared}, {Key: "A", Scope: core.ScopeProd},
		{Key: "B", Scope: core.ScopeShared}, {Key: "B", Scope: core.ScopePreview},
		{Key: "C", Scope: core.ScopeShared}, {Key: "C", Scope: core.ScopeProd}, {Key: "C", Scope: core.ScopePreview},
		{Key: "D", Scope: core.ScopeShared},
	}
	got := sharedEnvReach(vars)
	want := map[string]string{"A": "previews only", "B": "production only", "C": "unused"}
	if len(got) != len(want) {
		t.Fatalf("sharedEnvReach = %v, want %v", got, want)
	}
	for k, v := range want {
		if !strings.Contains(got[k], v) {
			t.Errorf("reach[%s] = %q, want it to say %q", k, got[k], v)
		}
	}
}
