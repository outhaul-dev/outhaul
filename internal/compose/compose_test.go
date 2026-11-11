package compose

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

func TestBuildArgs(t *testing.T) {
	got := buildArgs([]string{"docker-compose.yml", "outhaul.override.yml"}, "outhaul-web")
	want := []string{"compose", "-p", "outhaul-web", "-f", "docker-compose.yml", "-f", "outhaul.override.yml", "build"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestUpArgs(t *testing.T) {
	got := upArgs([]string{"docker-compose.yml"}, "outhaul-web", 90*time.Second)
	want := []string{"compose", "-p", "outhaul-web", "-f", "docker-compose.yml",
		"up", "-d", "--wait", "--wait-timeout", "90", "--remove-orphans"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upArgs = %v, want %v", got, want)
	}
	// A zero timeout (unset config) must still produce a valid flag value.
	got = upArgs([]string{"a.yml"}, "p", 0)
	if got[len(got)-3] != "--wait-timeout" || got[len(got)-2] != "1" {
		t.Errorf("zero timeout produced %v", got[len(got)-3:])
	}
}

func TestDockerMissingBinary(t *testing.T) {
	d := &Docker{Bin: "docker-does-not-exist-7a1b"}
	err := d.Stop(context.Background(), "outhaul-web", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compose v2 plugin") {
		t.Fatalf("expected an actionable docker-not-found error, got %v", err)
	}
}

func TestProjectName(t *testing.T) {
	if got := ProjectName("web"); got != "outhaul-web" {
		t.Errorf("ProjectName = %q", got)
	}
}

func TestOverride(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true},
	}

	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}))

	for _, want := range []string{
		"services:\n  \"web\":\n",
		"      \"outhaul\": {}\n",
		"      default: {}\n",
		`"traefik.enable": "true"`,
		`"traefik.docker.network": "outhaul"`,
		`"traefik.http.routers.outhaul-shop-d1.rule": ` + quoteYAML((traefik.Routing{}).Rule("shop.example.com", "")),
		`"traefik.http.services.outhaul-shop-d1.loadbalancer.server.port": "3000"`,
		"networks:\n  \"outhaul\":\n    external: true\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "certresolver") {
		t.Error("TLS labels must not appear when TLS is disabled")
	}

	tls := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}))
	if !strings.Contains(tls, `"traefik.http.routers.outhaul-shop-d1-tls.tls.certresolver": "le"`) {
		t.Errorf("TLS override missing certresolver labels; got:\n%s", tls)
	}
}

func TestOverridePathAndTLS(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 7, Host: "shop.example.com", Service: "api", Port: 8000, Path: "/api", InternalPath: "/", TLS: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}))
	if !strings.Contains(got, "PathPrefix(`/api`)") {
		t.Errorf("override missing path rule:\n%s", got)
	}
	if !strings.Contains(got, "stripprefix.prefixes") {
		t.Errorf("override missing strip middleware:\n%s", got)
	}
	if !strings.Contains(got, "websecure") {
		t.Errorf("override missing TLS router:\n%s", got)
	}
}

// TestOverrideMultipleDomains: several domains on one stack — two sharing a
// service, one on another service — render as one block per service, each
// domain with its own uniquely named router.
func TestOverrideMultipleDomains(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 7, Host: "shop.example.com", Service: "web", Port: 3000},
		{ID: 8, Host: "www.example.com", Service: "web", Port: 3000},
		{ID: 9, Host: "api.example.com", Service: "api", Port: 8080},
	}

	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}))

	for _, want := range []string{
		`"traefik.http.routers.outhaul-shop-d7.rule": ` + quoteYAML((traefik.Routing{}).Rule("shop.example.com", "")),
		`"traefik.http.routers.outhaul-shop-d8.rule": ` + quoteYAML((traefik.Routing{}).Rule("www.example.com", "")),
		`"traefik.http.routers.outhaul-shop-d9.rule": ` + quoteYAML((traefik.Routing{}).Rule("api.example.com", "")),
		`"traefik.http.services.outhaul-shop-d9.loadbalancer.server.port": "8080"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	// One services block per service, sorted: api before web, each declared once.
	if n := strings.Count(got, "  \"web\":\n"); n != 1 {
		t.Errorf("web service declared %d times, want once; got:\n%s", n, got)
	}
	apiIdx, webIdx := strings.Index(got, "  \"api\":\n"), strings.Index(got, "  \"web\":\n")
	if apiIdx < 0 || webIdx < 0 || apiIdx > webIdx {
		t.Errorf("services not rendered once each in sorted order; got:\n%s", got)
	}
	// Both web domains' labels live under the single web block.
	webBlock := got[webIdx:]
	if !strings.Contains(webBlock, "outhaul-shop-d7") || !strings.Contains(webBlock, "outhaul-shop-d8") {
		t.Errorf("web block missing one of its domains' routers; got:\n%s", webBlock)
	}
	if !strings.Contains(got[apiIdx:webIdx], "outhaul-shop-d9") {
		t.Errorf("api block missing its router; got:\n%s", got[apiIdx:webIdx])
	}
}

// A protected row puts the forward-auth gate on its service; an open row on the
// same service strips the identity header with an empty value, which must
// survive into the YAML as "" rather than vanish or become null.
func TestComposeOverrideCarriesGate(t *testing.T) {
	app := core.App{Name: "wiki"}
	domains := []core.Domain{
		{ID: 1, Host: "wiki.example.com", Service: "web", Port: 3000, TLS: true, Protected: true},
		{ID: 2, Host: "wiki.example.com", Service: "web", Port: 3000, Path: "/webhooks", TLS: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"}))
	for _, want := range []string{
		`"traefik.http.middlewares.outhaul-wiki-d1-gate.forwardauth.address": "http://host.docker.internal:8080/gate/check"`,
		`"traefik.http.middlewares.outhaul-wiki-d1-gate.forwardauth.authresponseheaders": "X-Outhaul-User"`,
		`"traefik.http.middlewares.outhaul-wiki-d1-gate.forwardauth.trustforwardheader": "false"`,
		"      \"traefik.http.middlewares.outhaul-wiki-d1-noalias.headers.customrequestheaders.X_Outhaul_User\": \"\"\n",
		`"traefik.http.routers.outhaul-wiki-d1.middlewares": "outhaul-wiki-d1-gate,outhaul-wiki-d1-noalias"`,
		`"traefik.http.routers.outhaul-wiki-d1-tls.middlewares": "outhaul-wiki-d1-gate,outhaul-wiki-d1-noalias"`,
		"      \"traefik.http.middlewares.outhaul-wiki-d2-noident.headers.customrequestheaders.X-Outhaul-User\": \"\"\n",
		"      \"traefik.http.middlewares.outhaul-wiki-d2-noident.headers.customrequestheaders.X_Outhaul_User\": \"\"\n",
		"      \"traefik.http.middlewares.outhaul-wiki-d2-noident.headers.customrequestheaders.X-Outhaul_User\": \"\"\n",
		"      \"traefik.http.middlewares.outhaul-wiki-d1-noalias.headers.customrequestheaders.X_Outhaul-User\": \"\"\n",
		`"traefik.http.routers.outhaul-wiki-d2.middlewares": "outhaul-wiki-d2-noident"`,
		// A CDN in front must not cache the protected row's responses.
		"      \"traefik.http.middlewares.outhaul-wiki-d1-noalias.headers.customresponseheaders.CDN-Cache-Control\": \"no-store\"\n",
		"      \"traefik.http.middlewares.outhaul-wiki-d1-noalias.headers.customresponseheaders.Cloudflare-CDN-Cache-Control\": \"no-store\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outhaul-wiki-d2-noident.headers.customresponseheaders") {
		t.Errorf("the open row sets a response header:\n%s", got)
	}
	if strings.Contains(got, "@file") {
		t.Errorf("override references the file provider:\n%s", got)
	}
}

// A Compose app's routers refuse ".." paths and keep their priority, as a
// single-container app's do: the override carries the same rule and priority
// labels, quoted so YAML gives back the rule's backslashes unchanged.
func TestComposeOverrideRefusesTraversal(t *testing.T) {
	app := core.App{Name: "wiki"}
	domains := []core.Domain{
		{ID: 1, Host: "wiki.example.com", Service: "web", Port: 3000, TLS: true, Protected: true},
		{ID: 2, Host: "wiki.example.com", Service: "web", Port: 3000, Path: "/webhooks", TLS: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}))
	for _, want := range []string{
		`"traefik.http.routers.outhaul-wiki-d1.rule": ` + quoteYAML((traefik.Routing{}).Rule("wiki.example.com", "")),
		`"traefik.http.routers.outhaul-wiki-d1-tls.rule": ` + quoteYAML((traefik.Routing{}).Rule("wiki.example.com", "")),
		`"traefik.http.routers.outhaul-wiki-d2.rule": ` + quoteYAML((traefik.Routing{}).Rule("wiki.example.com", "/webhooks")),
		`"traefik.http.routers.outhaul-wiki-d1.priority": "24"`,
		`"traefik.http.routers.outhaul-wiki-d1-tls.priority": "24"`,
		`"traefik.http.routers.outhaul-wiki-d2.priority": "51"`,
		`"traefik.http.routers.outhaul-wiki-d2-tls.priority": "51"`,
		`!PathRegexp(` + "`" + `(?i)(^|/|%2f|\\\\|%5c)(\\.|%2e){2}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %s; got:\n%s", want, got)
		}
	}
}

// Compose interpolates $ in every value of the files it reads, so the
// override writes a literal $ (the rule's "$" anchor) as $$, which Compose
// reads back as $.
func TestOverrideEscapesDollar(t *testing.T) {
	got := string(Override(core.App{Name: "wiki"}, []core.Domain{{ID: 1, Host: "wiki.example.com", Service: "web", Port: 3000}}, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}))
	if !strings.Contains(got, "%00|\\\\x00|$$)") {
		t.Errorf("the rule's $ should be written $$:\n%s", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "$$", ""), "$") {
		t.Errorf("a lone $ is left for Compose to interpolate:\n%s", got)
	}
	if q := quoteYAML(`a$b\"c`); q != `"a$$b\\\"c"` {
		t.Errorf("quoteYAML = %s", q)
	}
}

// The Compose override follows the same rule as single-container apps: on a
// TLS server a protected row gets its HTTPS router whatever its own TLS flag.
func TestComposeOverrideProtectedRowGetsHTTPSRouter(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true},
		{ID: 2, Host: "shop.example.com", Service: "web", Port: 3000, Path: "/admin", Protected: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}))
	for _, want := range []string{
		`"traefik.http.routers.outhaul-shop-d2-tls.middlewares": "outhaul-shop-d2-gate,outhaul-shop-d2-noalias"`,
		`"traefik.http.routers.outhaul-shop-d2-tls.tls.certresolver": "le"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %s; got:\n%s", want, got)
		}
	}
	if off := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"})); strings.Contains(off, "outhaul-shop-d2-tls") {
		t.Errorf("no HTTPS router expected without TLS on the server:\n%s", off)
	}
}

func TestOverrideCarriesTheHeader(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true, Protected: true},
		{ID: 2, Host: "shop.example.com", Path: "/api", Service: "web", Port: 3000, TLS: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{
		TLS: true, CertResolver: "le", GateAddress: "addr",
		HeaderName: "X-Azure-FDID", HeaderValue: "Ab0-._~+/=:",
	}))
	suffix := " && Header(`X-Azure-FDID`, `Ab0-._~+/=:`)"
	rules := 0
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, `.rule": `) {
			continue
		}
		rules++
		if !strings.HasSuffix(strings.TrimSpace(line), suffix+`"`) && !strings.Contains(line, suffix) {
			t.Errorf("rule line does not end with the header term: %s", line)
		}
	}
	if rules == 0 {
		t.Fatal("the override carries no rules")
	}
	if !strings.Contains(got, "Ab0-._~+/=:") {
		t.Error("the header value is not preserved in the override")
	}
}
