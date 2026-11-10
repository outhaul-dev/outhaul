package traefik

import (
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func router(app string, id int64) string { return RouterName(app, id) }

func TestRouteLabelsHostOnly(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "", "", false, "", false, "addr")
	if got["traefik.http.routers.r.rule"] != "Host(`web.example.com`)" {
		t.Errorf("rule = %q", got["traefik.http.routers.r.rule"])
	}
	if got["traefik.http.services.r.loadbalancer.server.port"] != "8080" {
		t.Errorf("port = %q", got["traefik.http.services.r.loadbalancer.server.port"])
	}
	if got["traefik.http.routers.r.middlewares"] != "r-noident" {
		t.Errorf("host-only open route middlewares = %q, want only the identity strip", got["traefik.http.routers.r.middlewares"])
	}
}

func TestRouteLabelsPathPrefixNoRewrite(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "", false, "", false, "addr")
	if got["traefik.http.routers.r.rule"] != "Host(`web.example.com`) && PathPrefix(`/api`)" {
		t.Errorf("rule = %q", got["traefik.http.routers.r.rule"])
	}
	if got["traefik.http.routers.r.middlewares"] != "r-noident" {
		t.Errorf("blank internal path should add no rewrite middleware: %q", got["traefik.http.routers.r.middlewares"])
	}
}

func TestRouteLabelsStripPrefix(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/", false, "", false, "addr")
	if got["traefik.http.routers.r.middlewares"] != "r-noident,r-strip" {
		t.Errorf("middlewares = %q, want r-noident,r-strip", got["traefik.http.routers.r.middlewares"])
	}
	if got["traefik.http.middlewares.r-strip.stripprefix.prefixes"] != "/api" {
		t.Errorf("stripprefix = %q", got["traefik.http.middlewares.r-strip.stripprefix.prefixes"])
	}
	if _, ok := got["traefik.http.middlewares.r-addpfx.addprefix.prefix"]; ok {
		t.Error("internal '/' should strip only, never add a prefix")
	}
}

func TestRouteLabelsStripThenAddPrefix(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/v1", false, "", false, "addr")
	if got["traefik.http.routers.r.middlewares"] != "r-noident,r-strip,r-addpfx" {
		t.Errorf("middlewares = %q, want r-noident,r-strip,r-addpfx", got["traefik.http.routers.r.middlewares"])
	}
	if got["traefik.http.middlewares.r-strip.stripprefix.prefixes"] != "/api" {
		t.Errorf("stripprefix = %q", got["traefik.http.middlewares.r-strip.stripprefix.prefixes"])
	}
	if got["traefik.http.middlewares.r-addpfx.addprefix.prefix"] != "/v1" {
		t.Errorf("addprefix = %q", got["traefik.http.middlewares.r-addpfx.addprefix.prefix"])
	}
}

func TestRouteLabelsTLSMirrorsRuleAndMiddlewares(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "/api", "/", true, "le", false, "addr")
	if got["traefik.http.routers.r-tls.entrypoints"] != "websecure" {
		t.Errorf("missing websecure entrypoint: %v", got)
	}
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver not set: %v", got)
	}
	if got["traefik.http.routers.r-tls.rule"] != "Host(`web.example.com`) && PathPrefix(`/api`)" {
		t.Errorf("secure rule = %q", got["traefik.http.routers.r-tls.rule"])
	}
	if got["traefik.http.routers.r-tls.middlewares"] != "r-noident,r-strip" {
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
	got := AppLabels(app, domains, 8080, false, "", "addr")
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
	got := AppLabels(core.App{Name: "web"}, nil, 8080, false, "", "addr")
	if got["traefik.enable"] != "false" {
		t.Errorf("app with no domains should disable Traefik: %v", got)
	}
}

func TestRouteLabelsLocalCANoResolver(t *testing.T) {
	got := RouteLabels("r", "web.local", 8080, "", "", true, "", false, "addr")
	if got["traefik.http.routers.r-tls.tls"] != "true" {
		t.Errorf("tls router missing: %v", got)
	}
	if _, ok := got["traefik.http.routers.r-tls.tls.certresolver"]; ok {
		t.Error("empty resolver must omit the certresolver label")
	}
}

func TestRouteLabelsACMEResolver(t *testing.T) {
	got := RouteLabels("r", "web.example.com", 8080, "", "", true, "le", false, "addr")
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver = %q; want le", got["traefik.http.routers.r-tls.tls.certresolver"])
	}
}

func TestAppLabelsPerRowTLS(t *testing.T) {
	app := core.App{Name: "web"}
	off := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, false, "", "addr")
	if _, ok := off["traefik.http.routers."+router("web", 1)+"-tls.tls"]; ok {
		t.Error("no TLS router expected when globalTLS is off")
	}
	on := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, true, "le", "addr")
	if on["traefik.http.routers."+router("web", 1)+"-tls.tls"] != "true" {
		t.Error("TLS router expected when both row.TLS and globalTLS are set")
	}
	opt := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: false}}, 8080, true, "le", "addr")
	if _, ok := opt["traefik.http.routers."+router("web", 1)+"-tls.tls"]; ok {
		t.Error("row that opted out of TLS should get no secure router")
	}
}

// Every domain row defines its own service on the same container. Traefik's
// Docker provider cannot auto-link a router to "the" service when a container
// defines several, and drops it — so on an HTTP-only or tunnel install (no
// TLS routers) a multi-domain app 404s on every domain. Each plain router must
// name its service explicitly.
func TestAppLabelsMultiDomainRoutersNameTheirService(t *testing.T) {
	app := core.App{Name: "minio"}
	domains := []core.Domain{
		{ID: 1, Host: "console.example.com"},
		{ID: 2, Host: "api.example.com"},
	}
	got := AppLabels(app, domains, 9000, false, "", "addr")
	for _, id := range []int64{1, 2} {
		r := router("minio", id)
		if svc := got["traefik.http.routers."+r+".service"]; svc != r {
			t.Errorf("router %s service = %q, want %q", r, svc, r)
		}
	}
}

// A protected row defines its forward-auth gate in its own labels. A router
// that references an undefined @file middleware is dropped by Traefik and the
// request falls through to an open router, so nothing may point at @file.
func TestProtectedRouteDefinesItsGateInLabels(t *testing.T) {
	l := RouteLabels("r", "wiki.example.com", 8080, "", "", true, "le", true, "http://host.docker.internal:8080/gate/check")
	want := map[string]string{
		"traefik.http.middlewares.r-gate.forwardauth.address":             "http://host.docker.internal:8080/gate/check",
		"traefik.http.middlewares.r-gate.forwardauth.authresponseheaders": "X-Outhaul-User",
		"traefik.http.routers.r.middlewares":                              "r-gate",
		"traefik.http.routers.r-tls.middlewares":                          "r-gate",
	}
	for k, v := range want {
		got, ok := l[k]
		if !ok {
			t.Errorf("missing label %s", k)
			continue
		}
		if got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for k, v := range l {
		if strings.Contains(v, "@file") {
			t.Errorf("%s = %q references the file provider", k, v)
		}
		if strings.Contains(strings.ToLower(k), "trustforwardheader") {
			t.Errorf("%s set: Traefik must overwrite X-Forwarded-* itself", k)
		}
	}
	// Forward auth already replaces the header, so a protected row needs no strip.
	if _, ok := l["traefik.http.middlewares.r-noident.headers.customrequestheaders.X-Outhaul-User"]; ok {
		t.Error("a protected row should not define the identity strip")
	}
}

// The gate must see the URL the visitor asked for, so it runs before any path
// rewrite on both routers.
func TestGateRunsBeforePathRewrites(t *testing.T) {
	l := RouteLabels("r", "h", 8080, "/a", "/", true, "", true, "addr")
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-gate,r-strip" {
		t.Errorf("routers.r.middlewares = %q, want r-gate,r-strip", got)
	}
	if got := l["traefik.http.routers.r-tls.middlewares"]; got != "r-gate,r-strip" {
		t.Errorf("routers.r-tls.middlewares = %q, want r-gate,r-strip", got)
	}
	l = RouteLabels("r", "h", 8080, "/a", "/v1", false, "", true, "addr")
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-gate,r-strip,r-addpfx" {
		t.Errorf("routers.r.middlewares = %q, want r-gate,r-strip,r-addpfx", got)
	}
}

// An open row removes any client-sent identity header, so an app that trusts
// it can't be fooled on a route that has no gate.
func TestOpenRouteStripsIdentityHeader(t *testing.T) {
	l := RouteLabels("r", "h", 8080, "", "", false, "", false, "addr")
	key := "traefik.http.middlewares.r-noident.headers.customrequestheaders.X-Outhaul-User"
	v, ok := l[key]
	if !ok {
		t.Fatalf("missing label %s", key)
	}
	if v != "" {
		t.Errorf("%s = %q, want empty (which removes the header)", key, v)
	}
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-noident" {
		t.Errorf("routers.r.middlewares = %q, want r-noident", got)
	}
	for k := range l {
		if strings.Contains(k, "forwardauth") {
			t.Errorf("open row carries forward-auth label %s", k)
		}
	}

	l = RouteLabels("r", "h", 8080, "/a", "/", true, "", false, "addr")
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-noident,r-strip" {
		t.Errorf("routers.r.middlewares = %q, want r-noident,r-strip", got)
	}
	if got := l["traefik.http.routers.r-tls.middlewares"]; got != "r-noident,r-strip" {
		t.Errorf("routers.r-tls.middlewares = %q, want r-noident,r-strip", got)
	}
}

func TestIdentityHeaderName(t *testing.T) {
	if IdentityHeader != "X-Outhaul-User" {
		t.Errorf("IdentityHeader = %q", IdentityHeader)
	}
}

func TestRowHasGate(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"protected, TLS", RouteLabels("r", "h", 8080, "", "", true, "le", true, "addr"), true},
		{"protected, no TLS", RouteLabels("r", "h", 8080, "", "", false, "", true, "addr"), true},
		{"protected with a rewrite", RouteLabels("r", "h", 8080, "/a", "/", true, "", true, "addr"), true},
		{"open, TLS", RouteLabels("r", "h", 8080, "", "", true, "le", false, "addr"), false},
		{"open, no TLS", RouteLabels("r", "h", 8080, "", "", false, "", false, "addr"), false},
		{"no labels", map[string]string{}, false},
		{"another router's gate", RouteLabels("other", "h", 8080, "", "", true, "", true, "addr"), false},
	}
	for _, c := range cases {
		if got := RowHasGate(c.labels, "r"); got != c.want {
			t.Errorf("%s: RowHasGate = %v, want %v", c.name, got, c.want)
		}
	}

	// The gate on r but not on r-tls: HTTPS visitors get through ungated.
	l := RouteLabels("r", "h", 8080, "", "", true, "", true, "addr")
	l["traefik.http.routers.r-tls.middlewares"] = "r-noident"
	if RowHasGate(l, "r") {
		t.Error("gate missing from r-tls should not count as gated")
	}
	delete(l, "traefik.http.routers.r-tls.middlewares")
	if RowHasGate(l, "r") {
		t.Error("r-tls with no middlewares should not count as gated")
	}
	// Traefik gives a router with no rule label a default rule, so a twin
	// with any label at all is a router that needs the gate.
	l = RouteLabels("r", "h", 8080, "", "", false, "", true, "addr")
	l["traefik.http.routers.r-tls.entrypoints"] = "websecure"
	if RowHasGate(l, "r") {
		t.Error("an ungated r-tls with no rule label should not count as gated")
	}

	// Listed, but with no forward-auth address the middleware isn't defined.
	l = RouteLabels("r", "h", 8080, "", "", true, "", true, "addr")
	delete(l, "traefik.http.middlewares.r-gate.forwardauth.address")
	if RowHasGate(l, "r") {
		t.Error("gate with no forward-auth address should not count as gated")
	}
	l["traefik.http.middlewares.r-gate.forwardauth.address"] = ""
	if RowHasGate(l, "r") {
		t.Error("gate with an empty forward-auth address should not count as gated")
	}

	// Listed, but not first: a rewrite would run before it.
	l = RouteLabels("r", "h", 8080, "/a", "/", false, "", true, "addr")
	l["traefik.http.routers.r.middlewares"] = "r-strip,r-gate"
	if RowHasGate(l, "r") {
		t.Error("gate listed after a rewrite should not count as gated")
	}
}

func TestAppLabelsCarryGateForProtectedRows(t *testing.T) {
	app := core.App{Name: "web"}
	domains := []core.Domain{
		{ID: 1, Host: "wiki.example.com", TLS: true, Protected: true},
		{ID: 2, Host: "wiki.example.com", Path: "/webhooks", TLS: true},
	}
	got := AppLabels(app, domains, 8080, true, "le", "http://host.docker.internal:9000/gate/check")
	if !RowHasGate(got, router("web", 1)) {
		t.Errorf("protected row 1 lacks its gate: %v", got)
	}
	if got["traefik.http.middlewares."+router("web", 1)+"-gate.forwardauth.address"] != "http://host.docker.internal:9000/gate/check" {
		t.Errorf("gate address not passed through: %v", got)
	}
	if RowHasGate(got, router("web", 2)) {
		t.Errorf("open row 2 has a gate: %v", got)
	}
	if got["traefik.http.routers."+router("web", 2)+".middlewares"] != router("web", 2)+"-noident" {
		t.Errorf("open row 2 should strip the identity header: %v", got)
	}
}
