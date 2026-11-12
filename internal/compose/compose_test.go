package compose

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
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

func TestConfigArgs(t *testing.T) {
	got := configArgs([]string{"docker-compose.yml"}, "outhaul-shop")
	want := []string{"compose", "-p", "outhaul-shop", "-f", "docker-compose.yml",
		"config", "--format", "json", "--no-env-resolution"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("configArgs = %v, want %v", got, want)
	}
}

// Pull fetches only images the server does not already have, and skips a
// service it builds, whose image has no registry tag to fetch.
func TestPullArgs(t *testing.T) {
	got := pullArgs([]string{"docker-compose.yml", "outhaul.override.yml"}, "outhaul-shop")
	want := []string{"compose", "-p", "outhaul-shop", "-f", "docker-compose.yml", "-f", "outhaul.override.yml",
		"pull", "--policy", "missing", "--ignore-buildable"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pullArgs = %v, want %v", got, want)
	}
}

// tempScript writes an executable shell script to a temp dir and returns its
// path, so Docker can be pointed at a stub in place of the real CLI.
func tempScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// --no-env-resolution arrived in Docker Compose 2.35. An older Compose rejects
// the flag; the error must name the version that works, not just the flag.
func TestDockerConfigNeedsCompose235(t *testing.T) {
	for _, stderr := range []string{
		"unknown flag: --no-env-resolution",
		"unknown shorthand flag: 'n' in --no-env-resolution",
	} {
		d := &Docker{Bin: tempScript(t, "#!/bin/sh\necho '"+stderr+"' >&2\nexit 1\n")}
		_, err := d.Config(context.Background(), "", []string{"docker-compose.yml"}, "outhaul-demo", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "Docker Compose 2.35 or newer") {
			t.Errorf("stderr %q: error = %v, want the Docker Compose 2.35 message", stderr, err)
		}
	}
}

func TestDockerConfig(t *testing.T) {
	d := &Docker{Bin: tempScript(t, `#!/bin/sh
printf '{"name":"demo"}\n'
echo 'warn: example.com is not resolvable' >&2
`)}
	var warn bytes.Buffer
	got, err := d.Config(context.Background(), "", []string{"docker-compose.yml"}, "outhaul-demo", &warn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"name\":\"demo\"}\n" {
		t.Errorf("Config stdout = %q", got)
	}
	if !strings.Contains(warn.String(), "example.com is not resolvable") {
		t.Errorf("Config warn = %q, want the script's stderr", warn.String())
	}
	if strings.Contains(warn.String(), `"name"`) {
		t.Errorf("Config warn = %q, want the JSON kept out of it", warn.String())
	}
}

// Any other failure keeps the ordinary compose error, so a broken compose file
// is not blamed on an old Compose.
func TestDockerConfigOtherFailure(t *testing.T) {
	d := &Docker{Bin: tempScript(t, "#!/bin/sh\necho 'unknown flag: --not-ours' >&2\nexit 1\n")}
	_, err := d.Config(context.Background(), "", []string{"docker-compose.yml"}, "outhaul-demo", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "docker compose config failed") {
		t.Fatalf("error = %v, want docker compose config failed", err)
	}
	if strings.Contains(err.Error(), "2.35") {
		t.Errorf("error = %v, want no version hint for an unrelated failure", err)
	}
}

// Compose's own reason for a load failure goes to stderr and the deploy log.
// The returned error carries its last line so the deployment's reason says
// what is wrong with the file. stdout, where a successful model would go, is
// never included.
func TestDockerConfigCarriesComposesMessage(t *testing.T) {
	d := &Docker{Bin: tempScript(t, `#!/bin/sh
printf '{"name":"model-must-not-leak"}\n'
echo 'warning: first line' >&2
echo 'validating /srv/app/docker-compose.yml: services.web.ports must be a list' >&2
exit 1
`)}
	_, err := d.Config(context.Background(), "", []string{"docker-compose.yml"}, "outhaul-demo", io.Discard)
	if err == nil {
		t.Fatal("Config returned no error")
	}
	for _, want := range []string{"docker compose config failed", "services.web.ports must be a list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	for _, unwanted := range []string{"warning: first line", "model-must-not-leak"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error = %v, want it without %q", err, unwanted)
		}
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

	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}, nil))

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

	tls := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}, nil))
	if !strings.Contains(tls, `"traefik.http.routers.outhaul-shop-d1-tls.tls.certresolver": "le"`) {
		t.Errorf("TLS override missing certresolver labels; got:\n%s", tls)
	}
}

func TestOverridePathAndTLS(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 7, Host: "shop.example.com", Service: "api", Port: 8000, Path: "/api", InternalPath: "/", TLS: true},
	}
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}, nil))
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

	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}, nil))

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
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"}, nil))
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
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}, nil))
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
	got := string(Override(core.App{Name: "wiki"}, []core.Domain{{ID: 1, Host: "wiki.example.com", Service: "web", Port: 3000}}, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}, nil))
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
	got := string(Override(app, domains, "outhaul", traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}, nil))
	for _, want := range []string{
		`"traefik.http.routers.outhaul-shop-d2-tls.middlewares": "outhaul-shop-d2-gate,outhaul-shop-d2-noalias"`,
		`"traefik.http.routers.outhaul-shop-d2-tls.tls.certresolver": "le"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %s; got:\n%s", want, got)
		}
	}
	if off := string(Override(app, domains, "outhaul", traefik.Routing{TLS: false, CertResolver: "", GateAddress: "addr"}, nil)); strings.Contains(off, "outhaul-shop-d2-tls") {
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
	}, nil))
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

func TestOverrideCarriesTheIngressStamp(t *testing.T) {
	rt := traefik.Routing{TLS: true, HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	app := core.App{Name: "shop"}
	domains := []core.Domain{{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true}}
	got := string(Override(app, domains, "outhaul", rt, nil))
	if !strings.Contains(got, `"`+traefik.IngressLabel+`": "`+rt.Stamp()+`"`) {
		t.Errorf("the override does not carry %s=%s:\n%s", traefik.IngressLabel, rt.Stamp(), got)
	}
}

var updateOverrideGolden = flag.Bool("update", false, "rewrite testdata/override.golden")

// The override for a stack with routed services is pinned byte for byte: an
// install that sets no limit and adds no extra must keep writing exactly this.
func TestOverrideGolden(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true, Protected: true},
		{ID: 2, Host: "shop.example.com", Path: "/webhooks", Service: "web", Port: 3000, TLS: true},
		{ID: 3, Host: "api.example.com", Service: "api", Port: 8080, TLS: true},
	}
	rt := traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"}
	got := Override(app, domains, "outhaul", rt, nil)

	path := filepath.Join("testdata", "override.golden")
	if *updateOverrideGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("override differs from %s (run with -update after an intended change):\n%s", path, got)
	}
}

func TestOverrideWithNoExtrasIsUnchanged(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{
		{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true, Protected: true},
		{ID: 2, Host: "shop.example.com", Path: "/webhooks", Service: "web", Port: 3000, TLS: true},
		{ID: 3, Host: "api.example.com", Service: "api", Port: 8080, TLS: true},
	}
	rt := traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "http://host.docker.internal:8080/gate/check"}
	got := Override(app, domains, "outhaul", rt, nil)
	want, err := os.ReadFile(filepath.Join("testdata", "override.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("override with nil extras differs from the golden file:\n%s", got)
	}
}

func TestOverrideWritesTopLevelLimits(t *testing.T) {
	got := string(Override(core.App{Name: "shop"}, nil, "outhaul", traefik.Routing{}, map[string]Extra{
		"web": {Limits: core.Limits{MilliCPUs: 500, Memory: 1 << 30, Pids: 512}},
	}))
	for _, want := range []string{
		"services:\n  \"web\":\n",
		"    cpus: 0.5\n",
		"    mem_limit: 1073741824\n",
		"    pids_limit: 512\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "deploy:") {
		t.Errorf("a service with no deploy block got one:\n%s", got)
	}
}

func TestOverrideWritesInsideAnExistingDeployBlock(t *testing.T) {
	got := string(Override(core.App{Name: "shop"}, nil, "outhaul", traefik.Routing{}, map[string]Extra{
		"web": {InDeploy: true, Limits: core.Limits{Memory: 1 << 30}},
	}))
	want := "    deploy:\n      resources:\n        limits:\n          memory: \"1073741824\"\n"
	if !strings.Contains(got, want) {
		t.Errorf("override missing the deploy limits block %q; got:\n%s", want, got)
	}
	if strings.Contains(got, "mem_limit") {
		t.Errorf("a service with a deploy limits block got a top-level mem_limit:\n%s", got)
	}
}

func TestOverrideAddsAServiceWithNoDomain(t *testing.T) {
	got := string(Override(core.App{Name: "shop"}, nil, "outhaul", traefik.Routing{}, map[string]Extra{
		"worker": {Limits: core.Limits{MilliCPUs: 500, Memory: 1 << 30, Pids: 512}},
	}))
	if !strings.Contains(got, "services:\n  \"worker\":\n") {
		t.Errorf("override missing the worker service; got:\n%s", got)
	}
	if strings.Contains(got, "networks:") {
		t.Errorf("a limits-only override joins the shared network:\n%s", got)
	}
	if strings.Contains(got, "labels:") {
		t.Errorf("a limits-only override writes labels:\n%s", got)
	}
}

func TestOverrideKeepsRoutedServices(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000}}
	got := string(Override(app, domains, "outhaul", traefik.Routing{}, map[string]Extra{
		"worker": {Limits: core.Limits{Pids: 512}},
	}))
	for _, want := range []string{
		"  \"web\":\n",
		"  \"worker\":\n",
		`"traefik.http.routers.outhaul-shop-d1.rule": `,
		"    pids_limit: 512\n",
		"networks:\n  \"outhaul\":\n    external: true\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q; got:\n%s", want, got)
		}
	}
}

// A service with no domain row gets the policy and nothing else, and no
// top-level networks: block appears because nothing joins the shared network.
func TestOverrideWritesTheRestartPolicy(t *testing.T) {
	got := string(Override(core.App{Name: "shop"}, nil, "outhaul", traefik.Routing{}, map[string]Extra{
		"worker": {Restart: "unless-stopped"},
	}))
	if !strings.Contains(got, "  \"worker\":\n    restart: \"unless-stopped\"\n") {
		t.Errorf("override missing worker's restart policy; got:\n%s", got)
	}
	if strings.Contains(got, "networks:") {
		t.Errorf("a restart-only override joins the shared network:\n%s", got)
	}
	if strings.Contains(got, "labels:") {
		t.Errorf("a restart-only override writes labels:\n%s", got)
	}
}

// The policy is the service block's first key, before the networks and labels a
// published service gets.
func TestOverrideWritesRestartBesideLabels(t *testing.T) {
	app := core.App{Name: "shop"}
	domains := []core.Domain{{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true}}
	rt := traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}
	got := string(Override(app, domains, "outhaul", rt, map[string]Extra{
		"web": {Restart: "unless-stopped"},
	}))
	if !strings.Contains(got, "  \"web\":\n    restart: \"unless-stopped\"\n    networks:\n") {
		t.Errorf("restart is not the service block's first key; got:\n%s", got)
	}
	if !strings.Contains(got, "Host(`shop.example.com`)") {
		t.Errorf("override lost the domain's router; got:\n%s", got)
	}
	if !strings.Contains(got, "networks:\n  \"outhaul\":\n    external: true\n") {
		t.Errorf("override lost the top-level networks block; got:\n%s", got)
	}
}

// One service can gain both an R7 limit and the policy; a service that only
// gains the policy keeps its block to the one line.
func TestOverrideWritesRestartAndLimits(t *testing.T) {
	domains := []core.Domain{{ID: 1, Host: "shop.example.com", Service: "web", Port: 3000, TLS: true}}
	extra := MergeExtras(
		map[string]Extra{"web": {Limits: core.Limits{MilliCPUs: 500}}},
		map[string]Extra{"web": {Restart: "unless-stopped"}, "worker": {Restart: "unless-stopped"}},
	)
	rt := traefik.Routing{TLS: true, CertResolver: "le", GateAddress: "addr"}
	got := string(Override(core.App{Name: "shop"}, domains, "outhaul", rt, extra))
	if !strings.Contains(got, "  \"web\":\n    restart: \"unless-stopped\"\n    networks:\n") {
		t.Errorf("web missing restart before its networks; got:\n%s", got)
	}
	if !strings.Contains(got, "    cpus: 0.5\n") {
		t.Errorf("web lost its limit; got:\n%s", got)
	}
	worker := got[strings.Index(got, "  \"worker\":\n"):]
	if !strings.HasPrefix(worker, "  \"worker\":\n    restart: \"unless-stopped\"\n") {
		t.Errorf("worker's block is not the policy alone; got:\n%s", worker)
	}
	if strings.Contains(worker, "labels:") || strings.Contains(worker, "cpus:") {
		t.Errorf("worker gained more than the policy; got:\n%s", worker)
	}
}
