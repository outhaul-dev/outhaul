package traefik

import (
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func router(app string, id int64) string { return RouterName(app, id) }

func TestRouteLabelsHostOnly(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "", "", false)
	if got["traefik.http.routers.r.rule"] != "Host(`web.example.com`)" {
		t.Errorf("rule = %q", got["traefik.http.routers.r.rule"])
	}
	if got["traefik.http.services.r.loadbalancer.server.port"] != "8080" {
		t.Errorf("port = %q", got["traefik.http.services.r.loadbalancer.server.port"])
	}
	if _, ok := got["traefik.http.routers.r.middlewares"]; ok {
		t.Error("host-only route should carry no middleware")
	}
}

func TestRouteLabelsPathPrefixNoRewrite(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "", false)
	if got["traefik.http.routers.r.rule"] != "Host(`web.example.com`) && PathPrefix(`/api`)" {
		t.Errorf("rule = %q", got["traefik.http.routers.r.rule"])
	}
	if _, ok := got["traefik.http.routers.r.middlewares"]; ok {
		t.Error("blank internal path should not add a middleware")
	}
}

func TestRouteLabelsStripPrefix(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/", false)
	if got["traefik.http.routers.r.middlewares"] != "r-strip" {
		t.Errorf("middlewares = %q, want r-strip", got["traefik.http.routers.r.middlewares"])
	}
	if got["traefik.http.middlewares.r-strip.stripprefix.prefixes"] != "/api" {
		t.Errorf("stripprefix = %q", got["traefik.http.middlewares.r-strip.stripprefix.prefixes"])
	}
	if _, ok := got["traefik.http.middlewares.r-addpfx.addprefix.prefix"]; ok {
		t.Error("internal '/' should strip only, never add a prefix")
	}
}

func TestRouteLabelsStripThenAddPrefix(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/v1", false)
	if got["traefik.http.routers.r.middlewares"] != "r-strip,r-addpfx" {
		t.Errorf("middlewares = %q, want r-strip,r-addpfx", got["traefik.http.routers.r.middlewares"])
	}
	if got["traefik.http.middlewares.r-strip.stripprefix.prefixes"] != "/api" {
		t.Errorf("stripprefix = %q", got["traefik.http.middlewares.r-strip.stripprefix.prefixes"])
	}
	if got["traefik.http.middlewares.r-addpfx.addprefix.prefix"] != "/v1" {
		t.Errorf("addprefix = %q", got["traefik.http.middlewares.r-addpfx.addprefix.prefix"])
	}
}

func TestRouteLabelsTLSMirrorsRuleAndMiddlewares(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/", true)
	if got["traefik.http.routers.r-tls.entrypoints"] != "websecure" {
		t.Errorf("missing websecure entrypoint: %v", got)
	}
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver not set: %v", got)
	}
	if got["traefik.http.routers.r-tls.rule"] != "Host(`web.example.com`) && PathPrefix(`/api`)" {
		t.Errorf("secure rule = %q", got["traefik.http.routers.r-tls.rule"])
	}
	if got["traefik.http.routers.r-tls.middlewares"] != "r-strip" {
		t.Errorf("secure router should reuse middlewares: %q", got["traefik.http.routers.r-tls.middlewares"])
	}
	if got["traefik.http.routers.r-tls.service"] != "r" {
		t.Errorf("secure router should reuse the service: %q", got["traefik.http.routers.r-tls.service"])
	}
}

func TestAppLabelsOneRouterPerDomain(t *testing.T) {
	app := core.App{Name: "web"}
	domains := []core.Domain{
		{ID: 1, Host: "a.example.com"},
		{ID: 2, Host: "b.example.com"},
	}
	got := AppLabels(app, domains, 8080, false)
	if got["traefik.enable"] != "true" || got["outhaul.app"] != "web" {
		t.Errorf("ownership markers missing: %v", got)
	}
	if got["traefik.http.routers."+router("web", 1)+".rule"] != "Host(`a.example.com`)" {
		t.Errorf("router for domain 1 wrong: %v", got)
	}
	if got["traefik.http.routers."+router("web", 2)+".rule"] != "Host(`b.example.com`)" {
		t.Errorf("router for domain 2 wrong: %v", got)
	}
}

func TestAppLabelsNoDomainsIsInternalOnly(t *testing.T) {
	got := AppLabels(core.App{Name: "web"}, nil, 8080, false)
	if got["traefik.enable"] != "false" {
		t.Errorf("app with no domains should disable Traefik: %v", got)
	}
}

func TestAppLabelsPerRowTLS(t *testing.T) {
	app := core.App{Name: "web"}
	off := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, false)
	if _, ok := off["traefik.http.routers."+router("web", 1)+"-tls.tls"]; ok {
		t.Error("no TLS router expected when globalTLS is off")
	}
	on := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, true)
	if on["traefik.http.routers."+router("web", 1)+"-tls.tls"] != "true" {
		t.Error("TLS router expected when both row.TLS and globalTLS are set")
	}
	opt := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: false}}, 8080, true)
	if _, ok := opt["traefik.http.routers."+router("web", 1)+"-tls.tls"]; ok {
		t.Error("row that opted out of TLS should get no secure router")
	}
}
