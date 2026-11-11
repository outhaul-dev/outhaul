package traefik

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestWildcardCovers(t *testing.T) {
	const wildcard = "*.shared.demo.example.com"
	cases := []struct {
		host string
		want bool
	}{
		{"app.shared.demo.example.com", true},
		{"APP.Shared.Demo.Example.COM", true},
		{"shared.demo.example.com", false},
		{"a.b.shared.demo.example.com", false},
		{"xshared.demo.example.com", false},
		{"app.shared.demo.example.com.", false},
		{"", false},
	}
	for _, c := range cases {
		if got := WildcardCovers(wildcard, c.host); got != c.want {
			t.Errorf("WildcardCovers(%q, %q) = %v, want %v", wildcard, c.host, got, c.want)
		}
	}
	if WildcardCovers("", "app.example.com") {
		t.Error("an empty wildcard covers nothing")
	}
}

func TestDNSChallengeFlags(t *testing.T) {
	pc := goldenProxyConfigs()["acme-no-admin"]
	pc.ACMEStaging = true
	pc.DNSProvider = "azuredns"
	pc.DNSEnv = []string{"AZURE_CLIENT_ID=abc"}
	spec := proxySpec(pc)
	joined := strings.Join(spec.Cmd, "\n")
	for _, want := range []string{
		"--certificatesresolvers.le.acme.dnschallenge=true",
		"--certificatesresolvers.le.acme.dnschallenge.provider=azuredns",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "httpchallenge") {
		t.Errorf("cmd still carries the HTTP challenge:\n%s", joined)
	}
	for _, want := range []string{
		"--certificatesresolvers.le.acme.email=ops@example.com",
		"--certificatesresolvers.le.acme.storage=/etc/traefik/acme/acme.json",
		"--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd missing %q:\n%s", want, joined)
		}
	}
	if !hasEnv(spec.Env, "AZURE_CLIENT_ID=abc") {
		t.Errorf("Env = %v, want AZURE_CLIENT_ID=abc", spec.Env)
	}
	for _, flag := range spec.Cmd {
		if strings.Contains(flag, "abc") {
			t.Errorf("a value leaked into a flag: %q", flag)
		}
	}
}

func TestDNSEnvChangesTheHash(t *testing.T) {
	pc := goldenProxyConfigs()["acme-no-admin"]
	pc.DNSProvider = "azuredns"
	pc.DNSEnv = []string{"AZURE_CLIENT_ID=abc"}
	first := proxySpec(pc).Labels["outhaul.config-hash"]
	pc.DNSEnv = []string{"AZURE_CLIENT_ID=xyz"}
	second := proxySpec(pc).Labels["outhaul.config-hash"]
	if first == second {
		t.Error("changing a DNS environment value should change the config hash")
	}
}

func TestWildcardEntrypointFlags(t *testing.T) {
	pc := goldenProxyConfigs()["acme-no-admin"]
	pc.Routing.Wildcard = "*.shared.demo.example.com"
	cmd := proxySpec(pc).Cmd
	var got []string
	for _, f := range cmd {
		if strings.HasPrefix(f, "--entrypoints.websecure.http.tls.") {
			got = append(got, f)
		}
	}
	want := []string{
		"--entrypoints.websecure.http.tls.certresolver=le",
		"--entrypoints.websecure.http.tls.domains[0].main=*.shared.demo.example.com",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wildcard flags = %v, want %v", got, want)
	}

	noWildcard := proxySpec(goldenProxyConfigs()["acme-no-admin"]).Cmd
	for _, f := range noWildcard {
		if strings.Contains(f, "entrypoints.websecure.http.tls") {
			t.Errorf("no wildcard, but cmd has %q", f)
		}
	}

	tunnel := goldenProxyConfigs()["tunnel-no-admin"]
	tunnel.Routing.Wildcard = "*.shared.demo.example.com"
	for _, f := range proxySpec(tunnel).Cmd {
		if strings.Contains(f, "entrypoints.websecure.http.tls") {
			t.Errorf("tunnel mode, but cmd has %q", f)
		}
	}
}

func TestCoveredHostInheritsTheWildcard(t *testing.T) {
	const wildcard = "*.shared.demo.example.com"
	covered := core.Domain{Host: "app.shared.demo.example.com", TLS: true}

	httpOn := RouteLabels("r", covered, 8080, Routing{TLS: true, CertResolver: "le", Wildcard: wildcard})
	if got := httpOn["traefik.http.routers.r-tls.entrypoints"]; got != "websecure" {
		t.Errorf("r-tls entrypoints = %q, want websecure", got)
	}
	for k := range httpOn {
		if strings.HasPrefix(k, "traefik.http.routers.r-tls.tls") {
			t.Errorf("a covered host should inherit the wildcard, but got label %q", k)
		}
	}

	httpOff := RouteLabels("r", covered, 8080, Routing{TLS: true, HTTPOff: true, CertResolver: "le", Wildcard: wildcard})
	if got := httpOff["traefik.http.routers.r.entrypoints"]; got != "websecure" {
		t.Errorf("HTTP off: routers.r.entrypoints = %q, want websecure", got)
	}
	for k := range httpOff {
		if strings.HasPrefix(k, "traefik.http.routers.r.tls") {
			t.Errorf("HTTP off: a covered host should inherit the wildcard, but got label %q", k)
		}
	}

	outside := RouteLabels("r", core.Domain{Host: "other.example.org", TLS: true}, 8080, Routing{TLS: true, CertResolver: "le", Wildcard: wildcard})
	if got := outside["traefik.http.routers.r-tls.tls"]; got != "true" {
		t.Errorf("a host outside the wildcard should ask for its own cert, tls = %q", got)
	}
	if got := outside["traefik.http.routers.r-tls.tls.certresolver"]; got != "le" {
		t.Errorf("certresolver = %q, want le", got)
	}
}

func TestAdminRouteUnderTheWildcard(t *testing.T) {
	const wildcard = "*.shared.demo.example.com"
	write := func(host string) string {
		pc := goldenProxyConfigs()["acme-no-admin"]
		pc.AdminHost = host
		pc.DynamicDir = t.TempDir()
		pc.Routing.Wildcard = wildcard
		if err := writeAdminDynamicConfig(pc); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if body := write("outhaul.shared.demo.example.com"); strings.Contains(body, "tls:") {
		t.Errorf("a covered admin host should inherit the wildcard:\n%s", body)
	}
	if body := write("outhaul.example.org"); !strings.Contains(body, "certResolver: le") {
		t.Errorf("an uncovered admin host should have its own resolver:\n%s", body)
	}
}

func TestRuleCarriesTheHeaderTerm(t *testing.T) {
	rt := Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	header := " && Header(`X-Azure-FDID`, `abc-123`)"
	if got, want := rt.Rule("wiki.example.com", "/api"), "Host(`wiki.example.com`) && PathPrefix(`/api`)"+traversalTerm+header; got != want {
		t.Errorf("Rule = %q, want %q", got, want)
	}
	if got, want := (Routing{}).Rule("wiki.example.com", "/api"), "Host(`wiki.example.com`) && PathPrefix(`/api`)"+traversalTerm; got != want {
		t.Errorf("Rule without a header = %q, want %q", got, want)
	}
}

func TestPriorityIgnoresTheHeaderTerm(t *testing.T) {
	rt := Routing{TLS: true, HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	labels := RouteLabels("r", core.Domain{Host: "wiki.example.com", Path: "/api", TLS: true}, 8080, rt)
	address := "Host(`wiki.example.com`) && PathPrefix(`/api`)"
	if got := labels["traefik.http.routers.r.priority"]; got != strconv.Itoa(len(address)) {
		t.Errorf("priority = %q, want %d", got, len(address))
	}
}

func TestRuleAddressWithHeader(t *testing.T) {
	rt := Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	for _, c := range []struct{ host, path string }{
		{"wiki.example.com", ""},
		{"wiki.example.com", "/api"},
	} {
		if h, p, ok := RuleAddress(rt.Rule(c.host, c.path)); !ok || h != c.host || p != c.path {
			t.Errorf("RuleAddress(%q) = %q, %q, %v; want %q, %q, true", rt.Rule(c.host, c.path), h, p, ok, c.host, c.path)
		}
	}
	for _, rule := range []string{
		rt.Rule("wiki.example.com", ""),
		Routing{HeaderName: "X-Azure-FDID", HeaderValue: "old-999"}.Rule("wiki.example.com", ""),
		Routing{HeaderName: "X-Other", HeaderValue: "abc-123"}.Rule("wiki.example.com", ""),
	} {
		if h, p, ok := RuleAddress(rule); !ok || h != "wiki.example.com" || p != "" {
			t.Errorf("RuleAddress(%q) = %q, %q, %v; want wiki.example.com, \"\", true", rule, h, p, ok)
		}
	}
	for _, rule := range []string{
		"Host(`h`) && Header(`X`, `v`)",
		"Host(`h`)" + traversalTerm + " && Header(`X`, `v`) && Header(`Y`, `w`)",
		"Host(`h`)" + traversalTerm + " && Header(`X`, `v`) && Method(`GET`)",
		"Host(`h`)" + traversalTerm + " && Header(`X`, ``)",
		"Host(`h`)" + traversalTerm + " && Header(``, `v`)",
	} {
		if h, p, ok := RuleAddress(rule); ok {
			t.Errorf("RuleAddress(%q) = %q, %q, true; want no address", rule, h, p)
		}
	}
	if !SameAddress(rt.Rule("wiki.example.com", ""), "wiki.example.com", "") {
		t.Error("SameAddress should compare only host and path")
	}
}

func TestEveryRouterCarriesTheHeader(t *testing.T) {
	rt := Routing{TLS: true, CertResolver: "le", GateAddress: goldenGate, HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	suffix := " && Header(`X-Azure-FDID`, `abc-123`)"
	check := func(name string, labels map[string]string) {
		for k, v := range labels {
			if strings.HasSuffix(k, ".rule") && !strings.HasSuffix(v, suffix) {
				t.Errorf("%s: %s = %q, does not end with the header term", name, k, v)
			}
		}
	}
	app := core.App{Name: "web"}
	rows := []core.Domain{
		{ID: 1, Host: "open.example.com"},
		{ID: 2, Host: "secure.example.com", TLS: true},
		{ID: 3, Host: "gated.example.com", Protected: true},
	}
	check("no TLS", AppLabels(app, rows, 8080, Routing{GateAddress: goldenGate, HeaderName: rt.HeaderName, HeaderValue: rt.HeaderValue}))
	check("HTTP on with TLS", AppLabels(app, rows, 8080, rt))
	httpOff := rt
	httpOff.HTTPOff = true
	check("HTTP off", AppLabels(app, rows, 8080, httpOff))

	for _, tunnel := range []bool{false, true} {
		pc := goldenProxyConfigs()["acme-no-admin"]
		pc.AdminHost = "outhaul.example.com"
		pc.DynamicDir = t.TempDir()
		pc.TunnelMode = tunnel
		pc.Routing = rt
		if err := writeAdminDynamicConfig(pc); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), suffix) {
			t.Errorf("tunnel %v: the admin rule does not end with the header term:\n%s", tunnel, data)
		}
	}
}

func TestHeaderKeepsTheGate(t *testing.T) {
	golden := loadRouteGolden(t)
	rt := Routing{TLS: true, CertResolver: "le", GateAddress: goldenGate, HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	for _, name := range []string{"protected-host", "protected-path"} {
		labels := RouteLabels("r", goldenRows[name], 8080, rt)
		if !RowHasGate(labels, "r") {
			t.Errorf("%s: RowHasGate = false", name)
		}
		if got, want := labels["traefik.http.routers.r.middlewares"], golden["acme/"+name]["traefik.http.routers.r.middlewares"]; got != want {
			t.Errorf("%s: middlewares = %q, want %q", name, got, want)
		}
	}
}
