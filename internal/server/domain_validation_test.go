package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Hosts end up inside a Traefik rule (Host(`%s`)), so anything that is not a
// bare lowercase hostname — a backtick, a port, a wildcard — must be refused
// before it is stored, on every path that accepts one.
var badHosts = []string{
	"a.com`)||HostRegexp(`.+",
	"shop.example.com:8080",
	"*.example.com",
	"bad_host.example.com",
}

// validateApp checks what will be stored; the create handler lowercases the
// submitted domain first, so an uppercase host reaching it is a caller bug.
func TestValidateAppRejectsNonHostnameDomain(t *testing.T) {
	for _, h := range append(badHosts, "Shop.Example.com") {
		app := core.App{Name: "shop", Kind: core.KindNixpacks, Source: core.SourcePublic, RepoURL: "https://example.com/r.git", Domain: h}
		if msg := validateApp(app); msg == "" {
			t.Errorf("validateApp accepted domain %q", h)
		}
	}
}

func TestParseDomainFieldsRejectsNonHostname(t *testing.T) {
	for _, h := range badHosts {
		if _, msg := parseDomainFields(h, "web", "80"); msg == "" {
			t.Errorf("parseDomainFields accepted domain %q", h)
		}
	}
}

func TestParseDomainFieldsLowercasesHost(t *testing.T) {
	d, msg := parseDomainFields(" Shop.Example.com ", "web", "80")
	if msg != "" {
		t.Fatalf("parseDomainFields rejected a mixed-case host: %s", msg)
	}
	if d.Host != "shop.example.com" {
		t.Fatalf("Host = %q, want lowercased", d.Host)
	}
}

func TestSavePreviewConfigRejectsBadBaseDomain(t *testing.T) {
	for _, bad := range []string{"x.com`)||HostRegexp(`.+", "preview.example.com/evil", "../../etc"} {
		t.Run(bad, func(t *testing.T) {
			e := newTestEnv(t)
			e.completeSetup(t)
			app, err := e.store.CreateApp(context.Background(), core.App{
				Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
			})
			if err != nil {
				t.Fatalf("CreateApp: %v", err)
			}
			res := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", url.Values{"enabled": {"on"}, "base_domain": {bad}})
			res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			cfg, err := e.store.GetPreviewConfig(context.Background(), app.ID)
			if err == nil && cfg.BaseDomain == bad {
				t.Fatalf("bad base domain was saved: %+v", cfg)
			}
		})
	}
}

func TestSavePreviewConfigLowercasesBaseDomain(t *testing.T) {
	e := newTestEnv(t)
	e.completeSetup(t)
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: "web", Source: core.SourceGithub, GithubRepo: "me/app", Kind: core.KindNixpacks, Branch: "main",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	res := e.postForm(t, "/apps/"+itoa(app.ID)+"/previews", url.Values{"enabled": {"on"}, "base_domain": {"Preview.Example.com"}})
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	cfg, err := e.store.GetPreviewConfig(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("GetPreviewConfig: %v", err)
	}
	if cfg.BaseDomain != "preview.example.com" {
		t.Fatalf("BaseDomain = %q, want lowercased", cfg.BaseDomain)
	}
}
