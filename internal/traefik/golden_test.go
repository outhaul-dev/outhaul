package traefik

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var goldenRows = map[string]core.Domain{
	"protected-host": {Host: "wiki.example.com", TLS: true, Protected: true},
	"open-path":      {Host: "wiki.example.com", Path: "/webhooks", TLS: true},
	"protected-path": {Host: "shop.example.com", Path: "/admin", InternalPath: "/", Protected: true},
	"open-host":      {Host: "plain.example.com"},
}

var goldenModes = map[string]struct {
	tls      bool
	resolver string
}{
	"no-tls":   {false, ""},
	"acme":     {true, "le"},
	"local-ca": {true, ""},
}

const goldenGate = "http://host.docker.internal:8080/gate/check"

type proxyGolden struct {
	Cmd         []string             `json:"cmd"`
	Ports       []docker.PortMapping `json:"ports"`
	Env         []string             `json:"env"`
	ExtraHosts  []string             `json:"extra_hosts"`
	ConfigHash  string               `json:"config_hash"`
	AdminConfig string               `json:"admin_config,omitempty"`
}

// TestRouteLabelsGolden pins every label RouteLabels writes, for every mode
// and row, so later changes can prove that an install which sets none of the
// new settings gets exactly the same routers.
func TestRouteLabelsGolden(t *testing.T) {
	path := filepath.Join("testdata", "route_labels.golden.json")
	got := map[string]map[string]string{}
	for mode, m := range goldenModes {
		for name, d := range goldenRows {
			got[mode+"/"+name] = RouteLabels("r", d, 8080, Routing{TLS: m.tls, CertResolver: m.resolver, GateAddress: goldenGate})
		}
	}
	checkGolden(t, path, got)
}

// TestProxySpecGolden pins Traefik's whole desired spec per mode, including
// the admin file-provider config, so a change that leaves the new settings
// unset cannot alter what an existing install runs.
func TestProxySpecGolden(t *testing.T) {
	path := filepath.Join("testdata", "proxy_spec.golden.json")
	got := map[string]proxyGolden{}
	base := goldenProxyConfigs()
	for name, pc := range base {
		for _, protected := range []bool{false, true} {
			pc.ProtectedRoutes = protected
			suffix := "-open"
			if protected {
				suffix = "-protected"
			}
			got[name+suffix] = takeProxyGolden(t, pc)
		}
	}
	for _, name := range []string{"acme", "localca", "tunnel"} {
		pc := base[name+"-no-admin"]
		pc.AdminHost = "outhaul.example.com"
		for _, protected := range []bool{false, true} {
			pc.ProtectedRoutes = protected
			suffix := "-open"
			if protected {
				suffix = "-protected"
			}
			got[name+"-admin"+suffix] = takeProxyGolden(t, pc)
		}
	}
	checkGolden(t, path, got)
}

func takeProxyGolden(t *testing.T, pc ProxyConfig) proxyGolden {
	t.Helper()
	spec := proxySpec(pc)
	g := proxyGolden{
		Cmd:        spec.Cmd,
		Ports:      spec.Ports,
		Env:        spec.Env,
		ExtraHosts: spec.ExtraHosts,
		ConfigHash: spec.Labels["outhaul.config-hash"],
	}
	if pc.adminRoutingEnabled() {
		dir := t.TempDir()
		writePC := pc
		writePC.DynamicDir = dir
		if err := writeAdminDynamicConfig(writePC); err != nil {
			t.Fatalf("write admin config for %s: %v", pc.ContainerName, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, adminDynamicFile))
		if err != nil {
			t.Fatalf("read admin config: %v", err)
		}
		g.AdminConfig = string(data)
	}
	return g
}

func checkGolden(t *testing.T, path string, got any) {
	t.Helper()
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run with -update to create it): %v", err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(data, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	gotJSON, _ := json.MarshalIndent(gotValue, "", "  ")
	wantJSON, _ := json.MarshalIndent(wantValue, "", "  ")
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("%s does not match the code's output; run with -update only when the change is intended.\n--- want\n%s\n--- got\n%s", path, wantJSON, gotJSON)
	}
}

func loadRouteGolden(t *testing.T) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "route_labels.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
