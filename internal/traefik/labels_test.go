package traefik

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func router(app string, id int64) string { return RouterName(app, id) }

func TestRouteLabelsHostOnly(t *testing.T) {
	got := RouteLabels("r", core.Domain{Host: "web.example.com", Path: "", InternalPath: "", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if got["traefik.http.routers.r.rule"] != (Routing{}).Rule("web.example.com", "") {
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
	got := RouteLabels("r", core.Domain{Host: "web.example.com", Path: "/api", InternalPath: "", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if got["traefik.http.routers.r.rule"] != (Routing{}).Rule("web.example.com", "/api") {
		t.Errorf("rule = %q", got["traefik.http.routers.r.rule"])
	}
	if got["traefik.http.routers.r.middlewares"] != "r-noident" {
		t.Errorf("blank internal path should add no rewrite middleware: %q", got["traefik.http.routers.r.middlewares"])
	}
}

func TestRouteLabelsStripPrefix(t *testing.T) {
	got := RouteLabels("r", core.Domain{Host: "web.example.com", Path: "/api", InternalPath: "/", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
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
	got := RouteLabels("r", core.Domain{Host: "web.example.com", Path: "/api", InternalPath: "/v1", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
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
	got := RouteLabels("r", core.Domain{Host: "web.example.com", Path: "/api", InternalPath: "/", TLS: true}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
	if got["traefik.http.routers.r-tls.entrypoints"] != "websecure" {
		t.Errorf("missing websecure entrypoint: %v", got)
	}
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver not set: %v", got)
	}
	if got["traefik.http.routers.r-tls.rule"] != (Routing{}).Rule("web.example.com", "/api") {
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
	got := AppLabels(app, domains, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if got["traefik.enable"] != "true" || got["outhaul.app"] != "web" {
		t.Errorf("ownership markers missing: %v", got)
	}
	if got["traefik.http.routers."+router("web", 1)+".rule"] != (Routing{}).Rule("a.example.com", "") {
		t.Errorf("router for domain 1 wrong: %v", got)
	}
	if got["traefik.http.routers."+router("web", 2)+".rule"] != (Routing{}).Rule("b.example.com", "") {
		t.Errorf("router for domain 2 wrong: %v", got)
	}
}

func TestAppLabelsNoDomainsIsInternalOnly(t *testing.T) {
	got := AppLabels(core.App{Name: "web"}, nil, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if got["traefik.enable"] != "false" {
		t.Errorf("app with no domains should disable Traefik: %v", got)
	}
}

func TestRouteLabelsLocalCANoResolver(t *testing.T) {
	got := RouteLabels("r", core.Domain{Host: "web.local", TLS: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
	if got["traefik.http.routers.r-tls.tls"] != "true" {
		t.Errorf("tls router missing: %v", got)
	}
	if _, ok := got["traefik.http.routers.r-tls.tls.certresolver"]; ok {
		t.Error("empty resolver must omit the certresolver label")
	}
}

func TestRouteLabelsACMEResolver(t *testing.T) {
	got := RouteLabels("r", core.Domain{Host: "web.example.com", TLS: true}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
	if got["traefik.http.routers.r-tls.tls.certresolver"] != "le" {
		t.Errorf("certresolver = %q; want le", got["traefik.http.routers.r-tls.tls.certresolver"])
	}
}

func TestAppLabelsPerRowTLS(t *testing.T) {
	app := core.App{Name: "web"}
	off := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if _, ok := off["traefik.http.routers."+router("web", 1)+"-tls.tls"]; ok {
		t.Error("no TLS router expected when globalTLS is off")
	}
	on := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: true}}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
	if on["traefik.http.routers."+router("web", 1)+"-tls.tls"] != "true" {
		t.Error("TLS router expected when both row.TLS and globalTLS are set")
	}
	opt := AppLabels(app, []core.Domain{{ID: 1, Host: "a.example.com", TLS: false}}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
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
	got := AppLabels(app, domains, 9000, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
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
	l := RouteLabels("r", core.Domain{Host: "wiki.example.com", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"})
	want := map[string]string{
		"traefik.http.middlewares.r-gate.forwardauth.address":             "http://host.docker.internal:8080/gate/check",
		"traefik.http.middlewares.r-gate.forwardauth.authresponseheaders": "X-Outhaul-User",
		// Explicitly false: Traefik then drops every X-Forwarded-* a client
		// sent, even through an entrypoint that trusts them, and sets its own.
		"traefik.http.middlewares.r-gate.forwardauth.trustforwardheader": "false",
		// The gate replaces X-Outhaul-User; the other spellings CGI-style
		// servers read as the same header need removing.
		"traefik.http.middlewares.r-noalias.headers.customrequestheaders.X_Outhaul_User": "",
		"traefik.http.middlewares.r-noalias.headers.customrequestheaders.X-Outhaul_User": "",
		"traefik.http.middlewares.r-noalias.headers.customrequestheaders.X_Outhaul-User": "",
		"traefik.http.routers.r.middlewares":                                             "r-gate,r-noalias",
		"traefik.http.routers.r-tls.middlewares":                                         "r-gate,r-noalias",
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
	}
	// Forward auth already replaces the header, so a protected row needs no strip.
	if _, ok := l["traefik.http.middlewares.r-noident.headers.customrequestheaders.X-Outhaul-User"]; ok {
		t.Error("a protected row should not define the identity strip")
	}
}

// The gate must see the URL the visitor asked for, so it runs before any path
// rewrite on both routers.
func TestGateRunsBeforePathRewrites(t *testing.T) {
	l := RouteLabels("r", core.Domain{Host: "h", Path: "/a", InternalPath: "/", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-gate,r-noalias,r-strip" {
		t.Errorf("routers.r.middlewares = %q, want r-gate,r-noalias,r-strip", got)
	}
	if got := l["traefik.http.routers.r-tls.middlewares"]; got != "r-gate,r-noalias,r-strip" {
		t.Errorf("routers.r-tls.middlewares = %q, want r-gate,r-noalias,r-strip", got)
	}
	l = RouteLabels("r", core.Domain{Host: "h", Path: "/a", InternalPath: "/v1", Protected: true}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-gate,r-noalias,r-strip,r-addpfx" {
		t.Errorf("routers.r.middlewares = %q, want r-gate,r-noalias,r-strip,r-addpfx", got)
	}
}

// An open row removes any client-sent identity header, so an app that trusts
// it can't be fooled on a route that has no gate.
func TestOpenRouteStripsIdentityHeader(t *testing.T) {
	l := RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	key := "traefik.http.middlewares.r-noident.headers.customrequestheaders.X-Outhaul-User"
	v, ok := l[key]
	if !ok {
		t.Fatalf("missing label %s", key)
	}
	if v != "" {
		t.Errorf("%s = %q, want empty (which removes the header)", key, v)
	}
	// Servers that turn headers into CGI-style variables read these spellings
	// as the same header, so they go too.
	for _, h := range []string{"X_Outhaul_User", "X-Outhaul_User", "X_Outhaul-User"} {
		alias := "traefik.http.middlewares.r-noident.headers.customrequestheaders." + h
		if v, ok := l[alias]; !ok || v != "" {
			t.Errorf("%s = %q (present %v), want present and empty", alias, v, ok)
		}
	}
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-noident" {
		t.Errorf("routers.r.middlewares = %q, want r-noident", got)
	}
	for k := range l {
		if strings.Contains(k, "forwardauth") || strings.Contains(k, "noalias") {
			t.Errorf("open row carries gate label %s", k)
		}
	}

	l = RouteLabels("r", core.Domain{Host: "h", Path: "/a", InternalPath: "/", TLS: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
	if got := l["traefik.http.routers.r.middlewares"]; got != "r-noident,r-strip" {
		t.Errorf("routers.r.middlewares = %q, want r-noident,r-strip", got)
	}
	if got := l["traefik.http.routers.r-tls.middlewares"]; got != "r-noident,r-strip" {
		t.Errorf("routers.r-tls.middlewares = %q, want r-noident,r-strip", got)
	}
}

// A CDN in front (a Cloudflare tunnel, or proxied DNS) must not cache what a
// protected route answers, or it would answer strangers without the gate: the
// route's response carries CDN-Cache-Control and Cloudflare-CDN-Cache-Control
// "no-store", set by its noalias headers middleware, after the gate. An open
// route's responses, and browsers' caching, are left as the app sets them.
func TestProtectedRouteTellsCDNsNotToCache(t *testing.T) {
	const prefix = "traefik.http.middlewares.r-noalias.headers.customresponseheaders."
	for _, tls := range []bool{false, true} {
		l := RouteLabels("r", core.Domain{Host: "wiki.example.com", Path: "/a", InternalPath: "/", Protected: true}, 8080, Routing{TLS: tls, CertResolver: "le", GateAddress: "addr"})
		for _, h := range []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control"} {
			if got := l[prefix+h]; got != "no-store" {
				t.Errorf("tls %v: %s%s = %q, want no-store", tls, prefix, h, got)
			}
		}
		for _, r := range []string{"r", "r-tls"} {
			if !tls && r == "r-tls" {
				continue
			}
			if got := l["traefik.http.routers."+r+".middlewares"]; got != "r-gate,r-noalias,r-strip" {
				t.Errorf("routers.%s.middlewares = %q, want the gate first, then r-noalias", r, got)
			}
		}
		if !RowHasGate(l, "r") {
			t.Error("RowHasGate = false for a protected route")
		}
		delete(l, prefix+"CDN-Cache-Control")
		delete(l, prefix+"Cloudflare-CDN-Cache-Control")
		if !RowHasGate(l, "r") {
			t.Error("RowHasGate requires the CDN headers; a route deployed before them still has its gate")
		}
	}
	for k := range RouteLabels("r", core.Domain{Host: "wiki.example.com", Path: "", InternalPath: "", Protected: false}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}) {
		if strings.Contains(strings.ToLower(k), "customresponseheaders") {
			t.Errorf("an open route sets a response header: %s", k)
		}
	}
}

func TestIdentityHeaderName(t *testing.T) {
	if IdentityHeader != "X-Outhaul-User" {
		t.Errorf("IdentityHeader = %q", IdentityHeader)
	}
	if IdentityHeaderAlias != "X_Outhaul_User" {
		t.Errorf("IdentityHeaderAlias = %q", IdentityHeaderAlias)
	}
}

func TestRowHasGate(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"protected, TLS", RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}), true},
		{"protected, no TLS", RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"}), true},
		{"protected with a rewrite", RouteLabels("r", core.Domain{Host: "h", Path: "/a", InternalPath: "/", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"}), true},
		{"open, TLS", RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: false}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}), false},
		{"open, no TLS", RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"}), false},
		{"no labels", map[string]string{}, false},
		{"another router's gate", RouteLabels("other", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"}), false},
	}
	for _, c := range cases {
		if got := RowHasGate(c.labels, "r"); got != c.want {
			t.Errorf("%s: RowHasGate = %v, want %v", c.name, got, c.want)
		}
	}

	// The gate on r but not on r-tls: HTTPS visitors get through ungated.
	l := RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
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
	l = RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	l["traefik.http.routers.r-tls.entrypoints"] = "websecure"
	if RowHasGate(l, "r") {
		t.Error("an ungated r-tls with no rule label should not count as gated")
	}

	// Listed, but with no forward-auth address the middleware isn't defined.
	l = RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
	delete(l, "traefik.http.middlewares.r-gate.forwardauth.address")
	if RowHasGate(l, "r") {
		t.Error("gate with no forward-auth address should not count as gated")
	}
	l["traefik.http.middlewares.r-gate.forwardauth.address"] = ""
	if RowHasGate(l, "r") {
		t.Error("gate with an empty forward-auth address should not count as gated")
	}

	// The gate is what counts: neither the trustForwardHeader label nor the
	// alias strip is required.
	l = RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: "addr"})
	delete(l, "traefik.http.middlewares.r-gate.forwardauth.trustforwardheader")
	l["traefik.http.routers.r.middlewares"] = "r-gate"
	l["traefik.http.routers.r-tls.middlewares"] = "r-gate"
	if !RowHasGate(l, "r") {
		t.Error("a gate without trustForwardHeader or the alias strip should still count as gated")
	}

	// Listed, but not first: a rewrite would run before it.
	l = RouteLabels("r", core.Domain{Host: "h", Path: "/a", InternalPath: "/", Protected: true}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
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
	got := AppLabels(app, domains, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:9000/gate/check"})
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

// On a TLS server the web entrypoint redirects everything to websecure, so a
// protected row's plain router never serves. Without its own HTTPS router an
// open HTTPS row on the same host would answer the path, ungated.
func TestProtectedRowGetsHTTPSRouterOnATLSServer(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", TLS: true},
		{ID: 2, Host: "shop.example.com", Path: "/admin", TLS: false, Protected: true},
		{ID: 3, Host: "plain.example.com", TLS: false},
	}
	got := AppLabels(app, domains, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
	admin := router("shop", 2)
	if got["traefik.http.routers."+admin+"-tls.entrypoints"] != "websecure" ||
		got["traefik.http.routers."+admin+"-tls.tls.certresolver"] != "le" {
		t.Errorf("protected row with its TLS flag off has no HTTPS router: %v", got)
	}
	if !RowHasGate(got, admin) {
		t.Errorf("protected row lacks its gate: %v", got)
	}
	if _, ok := got["traefik.http.routers."+router("shop", 3)+"-tls.rule"]; ok {
		t.Error("an open row with its TLS flag off should still get no HTTPS router")
	}
	// No TLS on the server (HTTP only, or the tunnel): no HTTPS routers at all.
	off := AppLabels(app, domains, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"})
	if _, ok := off["traefik.http.routers."+admin+"-tls.rule"]; ok {
		t.Error("no HTTPS router expected when the server has no TLS")
	}
	if !RowHasGate(off, admin) {
		t.Errorf("protected row lacks its gate without TLS: %v", off)
	}
}

// A protected row with no gate address still puts the gate first, so Traefik
// fails the route closed, and RowHasGate never reports it as gated.
func TestEmptyGateAddressFailsClosed(t *testing.T) {
	l := RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: true, CertResolver: "", GateAddress: ""})
	for _, r := range []string{"r", "r-tls"} {
		first, _, _ := strings.Cut(l["traefik.http.routers."+r+".middlewares"], ",")
		if first != "r-gate" {
			t.Errorf("routers.%s.middlewares = %q, want the gate first", r, l["traefik.http.routers."+r+".middlewares"])
		}
	}
	if _, ok := l["traefik.http.middlewares.r-noident.headers.customrequestheaders."+IdentityHeader]; ok {
		t.Error("a protected row with no gate address must not be treated as open")
	}
	if RowHasGate(l, "r") {
		t.Error("RowHasGate = true for a gate with no address")
	}
}

// While any container still carries a gate, Traefik may call it, so the
// proxy keeps the gate's host mapped. Any route's gate counts, on either
// router, whatever its address.
func TestHasGateLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"a protected row", AppLabels(core.App{Name: "web"}, []core.Domain{{ID: 1, Host: "h", Protected: true}}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}), true},
		{"a protected row among open ones", AppLabels(core.App{Name: "web"}, []core.Domain{{ID: 1, Host: "h"}, {ID: 2, Host: "h", Path: "/a", Protected: true}}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: "addr"}), true},
		{"a gate with no address", RouteLabels("r", core.Domain{Host: "h", Path: "", InternalPath: "", Protected: true}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: ""}), true},
		{"open rows only", AppLabels(core.App{Name: "web"}, []core.Domain{{ID: 1, Host: "h"}, {ID: 2, Host: "h", Path: "/a"}}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}), false},
		{"internal only", AppLabels(core.App{Name: "web"}, nil, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}), false},
		{"no labels", nil, false},
		{"a forward auth not named as a gate", map[string]string{"traefik.http.middlewares.auth.forwardauth.address": "x"}, false},
	}
	for _, c := range cases {
		if got := HasGateLabel(c.labels); got != c.want {
			t.Errorf("%s: HasGateLabel = %v, want %v", c.name, got, c.want)
		}
	}
}

// RuleAddress reads back the host and path of the rule RouteLabels writes, and
// of the rule an older Outhaul wrote (without the traversal term); anything
// else has no address.
func TestRuleAddress(t *testing.T) {
	for _, c := range []struct{ host, path string }{{"wiki.example.com", ""}, {"wiki.example.com", "/webhooks"}} {
		rule := RouteLabels("r", core.Domain{Host: c.host, Path: c.path, InternalPath: "", Protected: false}, 8080, Routing{TLS: false, CertResolver: "", GateAddress: ""})["traefik.http.routers.r.rule"]
		if rule != (Routing{}).Rule(c.host, c.path) {
			t.Errorf("(Routing{}).Rule(%q, %q) = %q, but RouteLabels wrote %q", c.host, c.path, (Routing{}).Rule(c.host, c.path), rule)
		}
		old := "Host(`" + c.host + "`)"
		if c.path != "" {
			old += " && PathPrefix(`" + c.path + "`)"
		}
		for _, r := range []string{rule, old} {
			host, path, ok := RuleAddress(r)
			if !ok || host != c.host || path != c.path {
				t.Errorf("RuleAddress(%q) = %q, %q, %v, want %q, %q, true", r, host, path, ok, c.host, c.path)
			}
		}
	}
	for _, rule := range []string{
		"", "PathPrefix(`/x`)", "Host(`unterminated", "Host(``)",
		"Host(`h`) && PathPrefix(``)",
		"Host(`h`) || Host(`evil`)",
		"Host(`h`) && PathPrefix(`/x`) && Method(`GET`)",
		"Host(`h`) && !PathRegexp(`.*`)",
		"Host(`h`) && PathPrefix(`/x`)" + traversalTerm + traversalTerm,
	} {
		if host, path, ok := RuleAddress(rule); ok {
			t.Errorf("RuleAddress(%q) = %q, %q, true, want no address", rule, host, path)
		}
	}
}

// Every rule refuses a path with a ".." segment in any spelling, so no row's
// router, open or protected, can be handed a path that leaves its prefix once
// an app decodes it.
func TestRuleRefusesTraversal(t *testing.T) {
	const term = " && !PathRegexp(`(?i)(^|/|%2f|\\\\|%5c)(\\.|%2e){2}(/|%2f|\\\\|%5c|;|%3b|%3f|%23|%00|\\x00|$)`)"
	if got, want := (Routing{}).Rule("wiki.example.com", ""), "Host(`wiki.example.com`)"+term; got != want {
		t.Errorf("(Routing{}).Rule(host) = %q, want %q", got, want)
	}
	if got, want := (Routing{}).Rule("wiki.example.com", "/webhooks"), "Host(`wiki.example.com`) && PathPrefix(`/webhooks`)"+term; got != want {
		t.Errorf("(Routing{}).Rule(host, path) = %q, want %q", got, want)
	}
	re := regexp.MustCompile(traversalPattern)
	// Paths as Traefik's PathRegexp sees them: escaped, with %2e and %00
	// already decoded and the hex upper-cased (checked on traefik:v3.7.6 by
	// deploy/test/integration_gate.sh), and in other spellings too.
	for _, p := range []string{
		"/webhooks%2F..%2Fpage", "/webhooks/..%2Fpage", "/x/..%2Fadmin", "/x/..%2fadmin",
		"/x/%2e%2e%2fadmin", "/x/%2E%2E%2Fadmin", "/x/.%2e/admin", "/x/..;/admin", "/x/..%3Badmin",
		"/x/..%5Cadmin", `/x/..\admin`, `\..\x`, "/x/../admin", "/x/..", "/..", "/webhooks/..%3B",
		"/webhooks/..%3Fx", "/webhooks/..%3fx", "/webhooks/..%23x", "/webhooks/..%00x", "/x/%2e%2e%3Fadmin",
		"/x/.%2E%23admin", "/..%23", "/..%00", "/webhooks/..\x00x", "/x/%2e%2e\x00admin", "/..\x00",
	} {
		if !re.MatchString(p) {
			t.Errorf("the traversal pattern lets %q through", p)
		}
	}
	for _, p := range []string{
		"/", "/a%2Fb", "/file..txt", "/.well-known/x", "/a/.../b", "/..foo", "/webhooks/x", "/x/..foo/y", "/a.b", "/%2e%2efoo",
		"/a%3Fb", "/a%23b", "/a%00b", "/a\x00b", "/file..%3Fx", "/x/...%23y", "/..%3", "/file..\x00",
	} {
		if re.MatchString(p) {
			t.Errorf("the traversal pattern refuses %q", p)
		}
	}
}

// Traefik orders routers by rule length unless told otherwise, and the
// traversal term makes every rule longer. Each router names, as its priority,
// the length of its rule without the term, which is the priority a container
// deployed before the term had, so old and new containers on one host order
// as they did.
func TestRoutersKeepTheirPriority(t *testing.T) {
	for _, c := range []struct{ host, path string }{{"wiki.example.com", ""}, {"wiki.example.com", "/webhooks"}} {
		old := "Host(`" + c.host + "`)"
		if c.path != "" {
			old += " && PathPrefix(`" + c.path + "`)"
		}
		want := strconv.Itoa(len(old))
		for _, protected := range []bool{false, true} {
			l := RouteLabels("r", core.Domain{Host: c.host, Path: c.path, TLS: true, Protected: protected}, 8080, Routing{TLS: true, CertResolver: "le", GateAddress: "addr"})
			for _, r := range []string{"r", "r-tls"} {
				if got := l["traefik.http.routers."+r+".priority"]; got != want {
					t.Errorf("%s%s (protected %v): routers.%s.priority = %q, want %q (the length of %s)", c.host, c.path, protected, r, got, want, old)
				}
			}
		}
	}
}
