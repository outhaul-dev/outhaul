package traefik

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/localca"
)

func testProxyConfig() ProxyConfig {
	return ProxyConfig{
		ContainerName: "outhaul-traefik",
		Image:         "traefik:v3.3",
		Network:       "outhaul",
		HTTPPort:      "80",
	}
}

func TestEnsureProxyCreatesNetworkAndContainer(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()

	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}

	if !f.Networks[pc.Network] {
		t.Errorf("network %q not created", pc.Network)
	}
	c, _ := f.FindContainer(ctx, pc.ContainerName)
	if c == nil {
		t.Fatal("traefik container not created")
	}
	if !c.Running() {
		t.Errorf("traefik container state = %q, want running", c.State)
	}
	if c.Image != pc.Image {
		t.Errorf("traefik image = %q, want %q", c.Image, pc.Image)
	}
	if len(f.Pulled) == 0 || f.Pulled[len(f.Pulled)-1] != pc.Image {
		t.Errorf("expected image %q to be pulled, pulled=%v", pc.Image, f.Pulled)
	}
}

func TestEnsureProxyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()

	for i := 0; i < 3; i++ {
		if err := EnsureProxy(ctx, f, pc, nil); err != nil {
			t.Fatalf("EnsureProxy call %d: %v", i, err)
		}
	}
	// Exactly one traefik container should exist.
	count := 0
	for _, c := range f.Containers {
		if c.Name == pc.ContainerName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 traefik container, found %d", count)
	}
}

func TestEnsureProxyStartsStoppedContainer(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()

	// A traefik container exists but is not running (e.g. after a host reboot
	// where restart policy did not fire).
	id, _ := f.CreateContainer(ctx, docker.ContainerSpec{Name: pc.ContainerName, Image: pc.Image})
	f.StopContainer(ctx, id, 0)

	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	c, _ := f.FindContainer(ctx, pc.ContainerName)
	if !c.Running() {
		t.Errorf("existing traefik container should be started, state=%q", c.State)
	}
}

func TestEnsureProxyConfiguresDockerProviderAndEntrypoint(t *testing.T) {
	// Capture the spec Traefik is created with by using a recording fake.
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := testProxyConfig()

	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	spec := rec.created
	joined := strings.Join(spec.Cmd, " ")
	if !strings.Contains(joined, "--providers.docker") {
		t.Errorf("traefik cmd missing docker provider: %v", spec.Cmd)
	}
	if !strings.Contains(joined, "exposedbydefault=false") {
		t.Errorf("traefik should not expose containers by default: %v", spec.Cmd)
	}
	if !strings.Contains(joined, ":80") {
		t.Errorf("traefik web entrypoint should bind :80: %v", spec.Cmd)
	}
	// Must publish the HTTP port on the host and mount the docker socket.
	foundPort := false
	for _, p := range spec.Ports {
		if p.HostPort == "80" && p.ContainerPort == "80" {
			foundPort = true
		}
	}
	if !foundPort {
		t.Errorf("traefik should publish host :80, ports=%v", spec.Ports)
	}
	foundSock := false
	for _, m := range spec.Mounts {
		if strings.Contains(m.Source, "docker.sock") {
			foundSock = true
		}
	}
	if !foundSock {
		t.Errorf("traefik should mount the docker socket, mounts=%v", spec.Mounts)
	}
}

func tlsProxyConfig() ProxyConfig {
	pc := testProxyConfig()
	pc.ACME = true
	pc.ACMEEmail = "ops@example.com"
	pc.HTTPSPort = "443"
	pc.ACMEStorageDir = "/var/lib/outhaul/traefik/acme"
	return pc
}

func TestEnsureProxyTLSFlagsAndMount(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureProxy(ctx, rec, tlsProxyConfig(), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	joined := strings.Join(rec.created.Cmd, " ")
	for _, want := range []string{
		"--entrypoints.websecure.address=:443",
		"--certificatesresolvers.le.acme.httpchallenge=true",
		"--certificatesresolvers.le.acme.email=ops@example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("traefik cmd missing %q; got %v", want, rec.created.Cmd)
		}
	}
	found443 := false
	for _, p := range rec.created.Ports {
		if p.HostPort == "443" && p.ContainerPort == "443" {
			found443 = true
		}
	}
	if !found443 {
		t.Errorf("traefik should publish :443, ports=%v", rec.created.Ports)
	}
	foundAcme := false
	for _, m := range rec.created.Mounts {
		if strings.Contains(m.Target, "/etc/traefik/acme") {
			foundAcme = true
		}
	}
	if !foundAcme {
		t.Errorf("traefik should mount the acme dir, mounts=%v", rec.created.Mounts)
	}
}

func TestEnsureProxyStagingUsesStagingCA(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := tlsProxyConfig()
	pc.ACMEStaging = true
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if !strings.Contains(strings.Join(rec.created.Cmd, " "), "acme-staging") {
		t.Errorf("staging CA server not configured: %v", rec.created.Cmd)
	}
}

func TestEnsureProxyRecreatesOnConfigDrift(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()

	if err := EnsureProxy(ctx, f, testProxyConfig(), nil); err != nil {
		t.Fatalf("EnsureProxy http: %v", err)
	}
	before, _ := f.FindContainer(ctx, "outhaul-traefik")

	if err := EnsureProxy(ctx, f, tlsProxyConfig(), nil); err != nil {
		t.Fatalf("EnsureProxy tls: %v", err)
	}
	after, _ := f.FindContainer(ctx, "outhaul-traefik")
	if after == nil {
		t.Fatal("traefik container missing after drift recreate")
	}
	if after.ID == before.ID {
		t.Error("expected traefik to be recreated on config drift (new container)")
	}
	if after.Labels["outhaul.config-hash"] == before.Labels["outhaul.config-hash"] {
		t.Error("config hash should differ after enabling TLS")
	}
}

func TestEnsureProxyRecreatesOnImageChange(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	before, _ := f.FindContainer(ctx, pc.ContainerName)

	pc.Image = "traefik:v3.4" // operator bumps the pinned image
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy after image bump: %v", err)
	}
	after, _ := f.FindContainer(ctx, pc.ContainerName)
	if after.ID == before.ID {
		t.Error("image bump should recreate the traefik container")
	}
	if after.Image != "traefik:v3.4" {
		t.Errorf("traefik image = %q, want traefik:v3.4", after.Image)
	}
}

func TestEnsureProxyKeepsOldContainerWhenPullFails(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureProxy(ctx, f, testProxyConfig(), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	before, _ := f.FindContainer(ctx, "outhaul-traefik")

	// Config drifts (TLS on) but the image pull now fails.
	f.FailPull = func(string) error { return errors.New("registry down") }
	if err := EnsureProxy(ctx, f, tlsProxyConfig(), nil); err == nil {
		t.Fatal("expected EnsureProxy to fail when the pull fails")
	}
	after, _ := f.FindContainer(ctx, "outhaul-traefik")
	if after == nil || after.ID != before.ID {
		t.Error("old traefik container must survive a failed pull (no teardown before pull)")
	}
}

func TestEnsureProxyPinsDockerAPIVersion(t *testing.T) {
	ctx := context.Background()

	// Explicit version is used verbatim.
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := testProxyConfig()
	pc.DockerAPIVersion = "1.51"
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if !hasEnv(rec.created.Env, "DOCKER_API_VERSION=1.51") {
		t.Errorf("want DOCKER_API_VERSION=1.51 pinned, env=%v", rec.created.Env)
	}

	// Empty falls back to the built-in default so the provider never breaks.
	rec2 := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureProxy(ctx, rec2, testProxyConfig(), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if !hasEnv(rec2.created.Env, "DOCKER_API_VERSION="+fallbackDockerAPIVersion) {
		t.Errorf("want fallback DOCKER_API_VERSION, env=%v", rec2.created.Env)
	}
}

func adminProxyConfig(t *testing.T) ProxyConfig {
	pc := tlsProxyConfig()
	pc.AdminHost = "outhaul.example.com"
	pc.AdminPort = "8080"
	pc.DynamicDir = t.TempDir()
	return pc
}

func TestEnsureProxyPublishesAdminRoute(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := adminProxyConfig(t)
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}

	joined := strings.Join(rec.created.Cmd, " ")
	if !strings.Contains(joined, "--providers.file.directory=/etc/traefik/dynamic") {
		t.Errorf("file provider not configured: %v", rec.created.Cmd)
	}
	if !hasEnv(rec.created.ExtraHosts, "host.docker.internal:host-gateway") {
		t.Errorf("host-gateway extra host missing: %v", rec.created.ExtraHosts)
	}
	foundMount := false
	for _, m := range rec.created.Mounts {
		if m.Target == "/etc/traefik/dynamic" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Errorf("dynamic dir not mounted: %v", rec.created.Mounts)
	}

	// The file-provider config must route the host to the admin UI with a cert.
	data, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
	if err != nil {
		t.Fatalf("read dynamic config: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"Host(`outhaul.example.com`)",
		"certResolver: le",
		"http://host.docker.internal:8080",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dynamic config missing %q; got:\n%s", want, body)
		}
	}
}

func TestEnsureProxyNoAdminRouteWithoutTLS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := adminProxyConfig(t)
	pc.ACME = false // a cert is impossible, so no admin route
	pc.ACMEEmail = ""
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if strings.Contains(strings.Join(rec.created.Cmd, " "), "providers.file") {
		t.Errorf("admin route should be absent without TLS: %v", rec.created.Cmd)
	}
	if _, err := os.Stat(filepath.Join(pc.DynamicDir, adminDynamicFile)); !os.IsNotExist(err) {
		t.Errorf("dynamic config should not be written without TLS (err=%v)", err)
	}
}

func TestWriteAdminDynamicConfigRemovesStaleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, adminDynamicFile)
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Routing now disabled (no AdminHost) — the stale route must be cleared.
	pc := ProxyConfig{ACME: true, DynamicDir: dir}
	if err := writeAdminDynamicConfig(pc); err != nil {
		t.Fatalf("writeAdminDynamicConfig: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale dynamic config should have been removed (err=%v)", err)
	}
}

func TestEnsureProxyRemovesStaleLocalCertsFileWhenLocalCAOff(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	dynDir := t.TempDir()
	path := filepath.Join(dynDir, localca.DynamicCertsFile)
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := testProxyConfig()
	pc.LocalCA = false
	pc.DynamicDir = dynDir
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale local-certs config should have been removed when LocalCA is off (err=%v)", err)
	}
}

func TestEnsureProxyKeepsLocalCertsFileWhenLocalCAOn(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	dynDir := t.TempDir()
	path := filepath.Join(dynDir, localca.DynamicCertsFile)
	if err := os.WriteFile(path, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := testProxyConfig()
	pc.LocalCA = true
	pc.CertsDir = t.TempDir()
	pc.DynamicDir = dynDir
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("local-certs config should survive when LocalCA is on: %v", err)
	}
	if string(body) != "kept" {
		t.Errorf("local-certs config should be untouched, got %q", body)
	}
}

func tunnelProxyConfig(t *testing.T) ProxyConfig {
	return ProxyConfig{
		ContainerName: "outhaul-traefik",
		Image:         "traefik:v3.3",
		Network:       "outhaul",
		HTTPPort:      "80",
		TunnelMode:    true,
		AdminHost:     "outhaul.example.com",
		AdminPort:     "8080",
		DynamicDir:    t.TempDir(),
	}
}

func TestEnsureProxyTunnelModeNoPortsNoTLS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureProxy(ctx, rec, tunnelProxyConfig(t), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if len(rec.created.Ports) != 0 {
		t.Errorf("tunnel mode must publish no host ports, got %v", rec.created.Ports)
	}
	joined := strings.Join(rec.created.Cmd, " ")
	for _, forbidden := range []string{":443", "acme", "redirections"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("tunnel-mode cmd must not contain %q: %v", forbidden, rec.created.Cmd)
		}
	}
	// The internal :80 web entrypoint is still needed (cloudflared reaches it).
	if !strings.Contains(joined, "--entrypoints.web.address=:80") {
		t.Errorf("web entrypoint missing: %v", rec.created.Cmd)
	}
}

func TestEnsureProxyTunnelModeAdminRouteNoTLS(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	pc := tunnelProxyConfig(t)
	if err := EnsureProxy(ctx, rec, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
	if err != nil {
		t.Fatalf("read dynamic config: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "Host(`outhaul.example.com`)") {
		t.Errorf("admin host rule missing:\n%s", body)
	}
	if strings.Contains(body, "certResolver") || strings.Contains(body, "websecure") {
		t.Errorf("tunnel-mode admin route must be plain HTTP (no TLS):\n%s", body)
	}
	if !strings.Contains(body, "- web") {
		t.Errorf("admin router should use the web entrypoint:\n%s", body)
	}
	if !strings.Contains(body, "http://host.docker.internal:8080") {
		t.Errorf("admin backend url missing:\n%s", body)
	}
}

func localCAProxyConfig(t *testing.T) ProxyConfig {
	pc := testProxyConfig()
	pc.LocalCA = true
	pc.HTTPSPort = "443"
	pc.CertsDir = t.TempDir()
	pc.DynamicDir = t.TempDir()
	return pc
}

func TestEnsureProxyLocalCA(t *testing.T) {
	ctx := context.Background()
	rec := &recordingFake{Fake: docker.NewFake()}
	if err := EnsureProxy(ctx, rec, localCAProxyConfig(t), nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	joined := strings.Join(rec.created.Cmd, " ")
	for _, want := range []string{
		"--entrypoints.websecure.address=:443",
		"--entrypoints.web.http.redirections.entrypoint.to=websecure",
		"--providers.file.directory=/etc/traefik/dynamic",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cmd missing %q; got %v", want, rec.created.Cmd)
		}
	}
	if strings.Contains(joined, "acme") {
		t.Errorf("local CA posture must carry no ACME flags: %v", rec.created.Cmd)
	}
	foundCerts := false
	for _, m := range rec.created.Mounts {
		if m.Target == "/etc/traefik/certs" && m.ReadOnly {
			foundCerts = true
		}
	}
	if !foundCerts {
		t.Errorf("certs dir not mounted read-only: %v", rec.created.Mounts)
	}
	found443 := false
	for _, p := range rec.created.Ports {
		if p.HostPort == "443" {
			found443 = true
		}
	}
	if !found443 {
		t.Errorf("should publish :443, ports=%v", rec.created.Ports)
	}
}

func TestAdminDynamicConfigLocalCA(t *testing.T) {
	pc := localCAProxyConfig(t)
	pc.AdminHost = "gateway.local"
	pc.AdminPort = "8080"
	if err := writeAdminDynamicConfig(pc); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "tls: {}") {
		t.Errorf("admin router should use tls: {} (file-provider cert), got:\n%s", body)
	}
	if strings.Contains(string(body), "certResolver") {
		t.Errorf("local CA admin router must not reference a certresolver:\n%s", body)
	}
}

// hasEnv reports whether want is present in the slice (used for Env/ExtraHosts).
func hasEnv(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}

// recordingFake wraps the Fake to capture the ContainerSpec passed to Create.
type recordingFake struct {
	*docker.Fake
	created docker.ContainerSpec
}

func (r *recordingFake) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	r.created = spec
	return r.Fake.CreateContainer(ctx, spec)
}

// goldenProxyConfigs are installs with no admin route and no protected route.
func goldenProxyConfigs() map[string]ProxyConfig {
	base := ProxyConfig{ContainerName: "outhaul-traefik", Image: "traefik:v3.7.6", Network: "outhaul", HTTPPort: "80",
		DockerSocket: "/var/run/docker.sock", DockerAPIVersion: "1.44", AdminPort: "8080", DynamicDir: "/var/lib/outhaul/traefik/dynamic"}
	plain := base
	acme := base
	acme.ACME, acme.ACMEEmail, acme.HTTPSPort, acme.ACMEStorageDir = true, "ops@example.com", "443", "/var/lib/outhaul/traefik/acme"
	localCA := base
	localCA.LocalCA, localCA.HTTPSPort, localCA.CertsDir = true, "443", "/var/lib/outhaul/traefik/certs"
	tunnel := base
	tunnel.TunnelMode = true
	return map[string]ProxyConfig{"plain": plain, "acme-no-admin": acme, "localca-no-admin": localCA, "tunnel-no-admin": tunnel}
}

// An install with no protected route and no admin route keeps exactly the
// Traefik it had before protected routes existed, so upgrading doesn't
// recreate it. The hashes were taken from the code before the change.
func TestProxyUnchangedWithoutProtectedRoutes(t *testing.T) {
	want := map[string]string{
		"plain":            "3eba453e3f94",
		"acme-no-admin":    "0050c4cc5af6",
		"localca-no-admin": "c5d7e52cbe1a",
		"tunnel-no-admin":  "0c01a78358e8",
	}
	for name, pc := range goldenProxyConfigs() {
		spec := proxySpec(pc)
		if got := spec.Labels["outhaul.config-hash"]; got != want[name] {
			t.Errorf("%s: config hash = %s, want %s (an upgrade would recreate Traefik)", name, got, want[name])
		}
		if len(spec.ExtraHosts) != 0 {
			t.Errorf("%s: extra hosts = %v, want none", name, spec.ExtraHosts)
		}
	}
}

// A protected route's gate is host.docker.internal on the admin port. Traefik
// must resolve that to the host whether or not the admin route is published,
// or the name would be looked up on the Docker network instead.
func TestProtectedRoutesMapTheGateHost(t *testing.T) {
	for name, pc := range goldenProxyConfigs() {
		pc.ProtectedRoutes = true
		if hosts := proxySpec(pc).ExtraHosts; len(hosts) != 1 || hosts[0] != "host.docker.internal:host-gateway" {
			t.Errorf("%s with a protected route: extra hosts = %v, want just the host gateway", name, hosts)
		}
	}
	pc := adminProxyConfig(t)
	pc.ProtectedRoutes = true
	if hosts := proxySpec(pc).ExtraHosts; len(hosts) != 1 || hosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("admin route and a protected route: extra hosts = %v, want the host gateway once", hosts)
	}
}

// findFails is a Docker that can't look up a container.
type findFails struct{ *docker.Fake }

func (findFails) FindContainer(context.Context, string) (*docker.Container, error) {
	return nil, errors.New("docker unreachable")
}

// ProxyMapsGateHost reads whether the running Traefik maps the gate's host,
// from its config hash: the one pc gives it with ProtectedRoutes on, or off.
// Anything else (no Traefik, a lookup that fails, a config that has drifted
// otherwise) is unknown.
func TestProxyMapsGateHost(t *testing.T) {
	ctx := context.Background()
	for _, on := range []bool{false, true} {
		dc := docker.NewFake()
		pc := testProxyConfig()
		pc.ProtectedRoutes = on
		if err := EnsureProxy(ctx, dc, pc, os.Stdout); err != nil {
			t.Fatal(err)
		}
		pc.ProtectedRoutes = !on // what is asked about doesn't matter
		if mapped, known := ProxyMapsGateHost(ctx, dc, pc); !known || mapped != on {
			t.Errorf("Traefik made with ProtectedRoutes %v: ProxyMapsGateHost = %v, %v; want %v, true", on, mapped, known, on)
		}
		pc.Image = "traefik:v9"
		if _, known := ProxyMapsGateHost(ctx, dc, pc); known {
			t.Error("a Traefik whose config differs otherwise: want unknown")
		}
		if _, known := ProxyMapsGateHost(ctx, findFails{dc}, testProxyConfig()); known {
			t.Error("a lookup that fails: want unknown")
		}
	}
	if _, known := ProxyMapsGateHost(ctx, docker.NewFake(), testProxyConfig()); known {
		t.Error("no Traefik: want unknown")
	}
	// With the admin route, the host is mapped either way.
	dc := docker.NewFake()
	pc := tunnelProxyConfig(t)
	if err := EnsureProxy(ctx, dc, pc, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if mapped, known := ProxyMapsGateHost(ctx, dc, pc); !known || !mapped {
		t.Errorf("admin route: ProxyMapsGateHost = %v, %v; want true, true", mapped, known)
	}
}

func TestHTTPOffPublishesOnly443(t *testing.T) {
	pc := localCAProxyConfig(t)
	pc.Routing = Routing{TLS: true, HTTPOff: true}
	spec := proxySpec(pc)
	want := []docker.PortMapping{{HostPort: "443", ContainerPort: "443", Proto: "tcp"}}
	if len(spec.Ports) != len(want) || spec.Ports[0] != want[0] {
		t.Errorf("ports = %v, want %v", spec.Ports, want)
	}
	for _, flag := range spec.Cmd {
		if strings.Contains(flag, "entrypoints.web.") {
			t.Errorf("HTTP-off spec names the web entrypoint: %q", flag)
		}
		if strings.Contains(flag, "redirections") {
			t.Errorf("HTTP-off spec carries a redirect: %q", flag)
		}
	}
}

func TestHTTPOffAdminRouteIsOnWebsecure(t *testing.T) {
	pc := localCAProxyConfig(t)
	pc.Routing = Routing{TLS: true, HTTPOff: true}
	pc.AdminHost = "outhaul.example.com"
	if err := writeAdminDynamicConfig(pc); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(pc.DynamicDir, adminDynamicFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "- websecure") {
		t.Errorf("admin route should use websecure:\n%s", body)
	}
	if strings.Contains(string(body), "- web\n") {
		t.Errorf("admin route names the web entrypoint with HTTP off:\n%s", body)
	}
}

func TestProxyFlagsAreTheSpecs(t *testing.T) {
	pc := tlsProxyConfig()
	spec := proxySpec(pc)
	cmd, env := ProxyFlags(pc)
	if strings.Join(cmd, "\n") != strings.Join(spec.Cmd, "\n") {
		t.Errorf("cmd = %v, want proxySpec's %v", cmd, spec.Cmd)
	}
	if strings.Join(env, "\n") != strings.Join(spec.Env, "\n") {
		t.Errorf("env = %v, want proxySpec's %v", env, spec.Env)
	}
}

// With a header required, a Traefik whose configuration differs is replaced
// from the local image if the pull fails: a change of flags alone never needs
// a pull.
func TestHeaderRequiredUsesTheLocalImageWhenThePullFails(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	f.Images = map[string]bool{testProxyConfig().Image: true}
	if err := EnsureProxy(ctx, f, testProxyConfig(), nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}
	f.FailPull = func(string) error { return errors.New("registry down") }
	pc := testProxyConfig()
	pc.Routing = Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy with a local image: %v", err)
	}
	c, _ := f.FindContainer(ctx, pc.ContainerName)
	if c == nil || !c.Running() {
		t.Fatalf("traefik is not running: %+v", c)
	}
	constraint := "--providers.docker.constraints=" + pc.Routing.Constraint()
	found := false
	for _, spec := range f.Created {
		if spec.Name != pc.ContainerName {
			continue
		}
		for _, flag := range spec.Cmd {
			if flag == constraint {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the new Traefik does not carry %q: %v", constraint, f.Created)
	}
}

// If the image isn't on the server either, a Traefik that can't be replaced
// must not go on serving containers without the header check.
func TestHeaderRequiredStopsTraefikItCannotReplace(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureProxy(ctx, f, testProxyConfig(), nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}
	f.FailPull = func(string) error { return errors.New("registry down") }
	pc := testProxyConfig()
	pc.Routing = Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	if err := EnsureProxy(ctx, f, pc, nil); err == nil {
		t.Fatal("EnsureProxy should fail when it cannot pull the image")
	}
	c, _ := f.FindContainer(ctx, pc.ContainerName)
	if c != nil && c.Running() {
		t.Error("the old Traefik is still running without the constraint")
	}
}

func TestGateHostMapping(t *testing.T) {
	pc := adminProxyConfig(t)
	pc.GateHost = "172.30.0.1"
	spec := proxySpec(pc)
	if len(spec.ExtraHosts) != 1 || spec.ExtraHosts[0] != "host.docker.internal:172.30.0.1" {
		t.Errorf("extra hosts = %v, want host.docker.internal:172.30.0.1", spec.ExtraHosts)
	}
	withGateway := spec.Labels["outhaul.config-hash"]
	pc.GateHost = ""
	if proxySpec(pc).Labels["outhaul.config-hash"] == withGateway {
		t.Error("changing GateHost should change the config hash")
	}

	none := adminProxyConfig(t)
	none.GateHost = "172.30.0.1"
	none.AdminHost = ""
	none.ProtectedRoutes = false
	if hosts := proxySpec(none).ExtraHosts; len(hosts) != 0 {
		t.Errorf("no admin route and no protected route: extra hosts = %v, want none", hosts)
	}
}

func TestProxyMapsGateHostWithAGateway(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := adminProxyConfig(t)
	pc.GateHost = "172.30.0.1"
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	mapped, known := ProxyMapsGateHost(ctx, f, pc)
	if !mapped || !known {
		t.Errorf("ProxyMapsGateHost = %v, %v; want true, true", mapped, known)
	}
}

// stopFails is a Docker whose container stop fails.
type stopFails struct{ *docker.Fake }

func (stopFails) StopContainer(context.Context, string, time.Duration) error {
	return errors.New("stop failed")
}

// If the old Traefik can't be stopped, the caller must hear about it: it is
// still serving without the required header.
func TestHeaderRequiredReportsAStopFailure(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	if err := EnsureProxy(ctx, f, testProxyConfig(), nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}
	f.FailPull = func(string) error { return errors.New("registry down") }
	pc := testProxyConfig()
	pc.Routing = Routing{HeaderName: "X-Azure-FDID", HeaderValue: "abc-123"}
	err := EnsureProxy(ctx, stopFails{f}, pc, nil)
	if err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("EnsureProxy = %v, want an error naming the failed stop", err)
	}
}

func trustedFlags(spec docker.ContainerSpec, ep string) (trustedips, insecure, middleware bool, ips string) {
	for _, f := range spec.Cmd {
		switch {
		case strings.HasPrefix(f, "--entrypoints."+ep+".forwardedheaders.trustedips="):
			trustedips = true
			ips = strings.TrimPrefix(f, "--entrypoints."+ep+".forwardedheaders.trustedips=")
		case f == "--entrypoints."+ep+".forwardedheaders.insecure=true":
			insecure = true
		case f == "--entrypoints."+ep+".http.middlewares=outhaul-forwarded@file":
			middleware = true
		}
	}
	return
}

func dropsUnderscoreHeaders(spec docker.ContainerSpec, ep string) bool {
	return slices.Contains(spec.Cmd, "--entrypoints."+ep+".http.underscoreheadersstrategy=delete")
}

func TestProxySpecTrustsAddresses(t *testing.T) {
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	pc.TrustedProxies = []string{"192.0.2.0/24", "2001:db8::1"}
	spec := proxySpec(pc)
	for _, ep := range []string{"web", "websecure"} {
		if !slices.Contains(spec.Cmd, "--entrypoints."+ep+".forwardedheaders.trustedips=192.0.2.0/24,2001:db8::1") {
			t.Errorf("%s: trustedips flag missing: %v", ep, spec.Cmd)
		}
		if !slices.Contains(spec.Cmd, "--entrypoints."+ep+".http.middlewares=outhaul-forwarded@file") {
			t.Errorf("%s: middleware flag missing: %v", ep, spec.Cmd)
		}
		if !dropsUnderscoreHeaders(spec, ep) {
			t.Errorf("%s: underscore-headers flag missing: %v", ep, spec.Cmd)
		}
	}
	for _, f := range spec.Cmd {
		if strings.Contains(f, "forwardedheaders.insecure") {
			t.Errorf("unexpected insecure flag %q", f)
		}
	}
	for _, want := range []string{"--providers.file.directory=/etc/traefik/dynamic", "--providers.file.watch=true"} {
		if !slices.Contains(spec.Cmd, want) {
			t.Errorf("missing %q: %v", want, spec.Cmd)
		}
	}
	want := docker.Mount{Source: pc.DynamicDir, Target: "/etc/traefik/dynamic", ReadOnly: true}
	if !slices.Contains(spec.Mounts, want) {
		t.Errorf("mounts = %v, want %v", spec.Mounts, want)
	}
}

func TestProxySpecTrustsAll(t *testing.T) {
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	pc.TrustAllProxies = true
	spec := proxySpec(pc)
	for _, ep := range []string{"web", "websecure"} {
		if !slices.Contains(spec.Cmd, "--entrypoints."+ep+".forwardedheaders.insecure=true") {
			t.Errorf("%s: insecure flag missing: %v", ep, spec.Cmd)
		}
		if !dropsUnderscoreHeaders(spec, ep) {
			t.Errorf("%s: underscore-headers flag missing: %v", ep, spec.Cmd)
		}
	}
	for _, f := range spec.Cmd {
		if strings.Contains(f, "forwardedheaders.trustedips") {
			t.Errorf("unexpected trustedips flag %q", f)
		}
	}
}

func TestProxySpecTrustedEntrypoints(t *testing.T) {
	tunnel := testProxyConfig()
	tunnel.TunnelMode = true
	httpOff := tlsProxyConfig()
	httpOff.Routing.HTTPOff = true
	cases := []struct {
		name string
		pc   ProxyConfig
		want []string
	}{
		{"plain HTTP", testProxyConfig(), []string{"web"}},
		{"ACME", tlsProxyConfig(), []string{"web", "websecure"}},
		{"local CA", localCAProxyConfig(t), []string{"web", "websecure"}},
		{"tunnel", tunnel, []string{"web"}},
		{"HTTP off", httpOff, []string{"websecure"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := tc.pc
			pc.DynamicDir = t.TempDir()
			pc.TrustedProxies = []string{"192.0.2.1"}
			spec := proxySpec(pc)
			for _, ep := range []string{"web", "websecure"} {
				ips, _, mw, got := trustedFlags(spec, ep)
				us := dropsUnderscoreHeaders(spec, ep)
				if slices.Contains(tc.want, ep) {
					if !ips || !mw || !us || got != "192.0.2.1" {
						t.Errorf("%s: want trust flags, got trustedips=%v middleware=%v underscore=%v: %v", ep, ips, mw, us, spec.Cmd)
					}
				} else if ips || mw || us {
					t.Errorf("%s: want no trust flags: %v", ep, spec.Cmd)
				}
			}
			for _, f := range spec.Cmd {
				if strings.Contains(f, "underscoreheadersstrategy") && !slices.Contains(tc.want, strings.SplitN(strings.TrimPrefix(f, "--entrypoints."), ".", 2)[0]) {
					t.Errorf("underscore flag %q is on an entrypoint that trusts nobody", f)
				}
			}
		})
	}
}

func TestProxySpecUnsetTrustAddsNoUnderscoreFlag(t *testing.T) {
	for _, f := range proxySpec(tlsProxyConfig()).Cmd {
		if strings.Contains(f, "underscoreheadersstrategy") {
			t.Errorf("unexpected flag %q with no proxy trusted", f)
		}
	}
}

func TestForwardedDynamicConfigBlanksTheHeaders(t *testing.T) {
	got := ForwardedDynamicConfig()
	for _, h := range []string{
		"X-Real-Ip", "X-Forwarded-Uri", "X-Forwarded-Method", "X-Forwarded-Prefix",
		"X-Forwarded-Tls-Client-Cert", "X-Forwarded-Tls-Client-Cert-Info",
	} {
		if !strings.Contains(got, "          "+h+": \"\"\n") {
			t.Errorf("config does not blank %s:\n%s", h, got)
		}
	}
}

func TestProxySpecHashChangesWithTrust(t *testing.T) {
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	base := proxySpec(pc).Labels["outhaul.config-hash"]
	pc.TrustedProxies = []string{"192.0.2.1"}
	named := proxySpec(pc).Labels["outhaul.config-hash"]
	if named == base {
		t.Error("naming a proxy did not change the hash")
	}
	pc.TrustedProxies = nil
	pc.TrustAllProxies = true
	all := proxySpec(pc).Labels["outhaul.config-hash"]
	if all == base || all == named {
		t.Errorf("trusting all gave hash %s, same as base %s or named %s", all, base, named)
	}
}

func TestEnsureProxyWritesAndRemovesForwardedConfig(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	pc.TrustedProxies = []string{"192.0.2.1"}
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	path := filepath.Join(pc.DynamicDir, forwardedDynamicFile)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("forwarded config not written: %v", err)
	}
	if string(got) != ForwardedDynamicConfig() {
		t.Errorf("forwarded config = %q, want %q", got, ForwardedDynamicConfig())
	}
	pc.TrustedProxies = nil
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy (trust off): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("forwarded config should be removed, stat err = %v", err)
	}
}

func TestEnsureProxyRewritesForwardedConfigWithoutATmpFile(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	pc.TrustedProxies = []string{"192.0.2.1"}
	path := filepath.Join(pc.DynamicDir, forwardedDynamicFile)
	for i := 0; i < 2; i++ {
		if err := EnsureProxy(ctx, f, pc, nil); err != nil {
			t.Fatalf("EnsureProxy #%d: %v", i+1, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != ForwardedDynamicConfig() {
			t.Fatalf("after #%d: config = %q, err = %v", i+1, got, err)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("after #%d: tmp file left behind, stat err = %v", i+1, err)
		}
	}
	// A file that holds something else is replaced whole.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy (stale): %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != ForwardedDynamicConfig() {
		t.Errorf("stale config not replaced: %q", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file left behind, stat err = %v", err)
	}
}

func TestEnsureProxyKeepsForwardedConfigWhenThePullFailsWhileTurningItOff(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := tlsProxyConfig()
	pc.DynamicDir = t.TempDir()
	pc.TrustedProxies = []string{"192.0.2.1"}
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	path := filepath.Join(pc.DynamicDir, forwardedDynamicFile)
	pc.TrustedProxies = nil
	f.FailPull = func(string) error { return errors.New("registry down") }
	if err := EnsureProxy(ctx, f, pc, nil); err == nil {
		t.Fatal("EnsureProxy should fail when the pull fails")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the old Traefik still names the middleware, so the file must stay: %v", err)
	}
	after, _ := f.FindContainer(ctx, pc.ContainerName)
	if after == nil || !after.Running() {
		t.Errorf("the old Traefik should still be running: %+v", after)
	}
}

func TestEnsureProxyTrustedNeedsDynamicDir(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := tlsProxyConfig()
	pc.TrustedProxies = []string{"192.0.2.1"}
	pc.DynamicDir = ""
	err := EnsureProxy(ctx, f, pc, nil)
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_TRUSTED_PROXIES") {
		t.Fatalf("EnsureProxy = %v, want an error naming OUTHAUL_TRUSTED_PROXIES", err)
	}
	if f.Networks[pc.Network] {
		t.Error("a network was created")
	}
	if c, _ := f.FindContainer(ctx, pc.ContainerName); c != nil {
		t.Error("a container was created")
	}
}

func TestProxySpecCarriesTheAddress(t *testing.T) {
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"
	spec := proxySpec(pc)
	if spec.IPv4 != "172.30.0.2" {
		t.Errorf("spec IPv4 = %q, want %q", spec.IPv4, "172.30.0.2")
	}
	if len(spec.Networks) != 1 || spec.Networks[0] != pc.Network {
		t.Errorf("spec networks = %v, want just %q", spec.Networks, pc.Network)
	}
	if got := proxySpec(testProxyConfig()).IPv4; got != "" {
		t.Errorf("spec IPv4 with no address = %q, want it left to Docker", got)
	}
}

// The hash decides whether Traefik is recreated. The address is part of the
// container, so changing it must recreate. The subnet is not: the network is
// checked on its own, and hashing it would recreate Traefik unnecessarily.
func TestProxyHashChangesWithTheAddress(t *testing.T) {
	hash := func(subnet, ipv4 string) string {
		pc := testProxyConfig()
		pc.Subnet, pc.IPv4 = subnet, ipv4
		return proxySpec(pc).Labels["outhaul.config-hash"]
	}
	plain := hash("", "")
	if got := hash("", "172.30.0.2"); got == plain {
		t.Errorf("config hash with an address = %s, want it to differ from %s", got, plain)
	}
	if got := hash("172.30.0.0/24", ""); got != plain {
		t.Errorf("config hash with only a subnet = %s, want %s (a subnet alone must not recreate Traefik)", got, plain)
	}
	if got, want := hash("172.30.0.0/24", "172.30.0.2"), hash("", "172.30.0.2"); got != want {
		t.Errorf("config hash with a subnet and an address = %s, want %s (the subnet is not part of it)", got, want)
	}
	if hash("", "172.30.0.3") == hash("", "172.30.0.2") {
		t.Error("a different address must change the config hash")
	}
}

// A fixed address is only reserved by the network's fixed range, so it is
// meaningless without a subnet: refuse it here too, not only in config.Validate,
// because a ProxyConfig can be built without going through Load.
func TestEnsureProxyRefusesAnAddressWithoutASubnet(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.IPv4 = "172.30.0.2"

	err := EnsureProxy(ctx, f, pc, nil)
	if err == nil || !strings.Contains(err.Error(), "OUTHAUL_NETWORK_SUBNET") {
		t.Fatalf("EnsureProxy = %v, want an error naming the missing subnet", err)
	}
	if len(f.NetworkSpecs) != 0 || len(f.Created) != 0 || len(f.Pulled) != 0 {
		t.Errorf("network specs %+v, created %+v, pulled %v; want no side effects", f.NetworkSpecs, f.Created, f.Pulled)
	}
}

func TestEnsureProxyUsesTheNetworkSpec(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"

	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}

	want := docker.NetworkSpec{Name: "outhaul", Subnet: "172.30.0.0/24", Gateway: "172.30.0.1", IPRange: "172.30.0.128/25"}
	if len(f.NetworkSpecs) != 1 || f.NetworkSpecs[0] != want {
		t.Fatalf("network specs = %+v, want [%+v]", f.NetworkSpecs, want)
	}
	if len(f.Created) != 1 || f.Created[0].IPv4 != "172.30.0.2" {
		t.Fatalf("created specs = %+v, want one with IPv4 %q", f.Created, "172.30.0.2")
	}
}

func TestEnsureProxyRefusesAnAddressAnotherContainerHolds(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"
	f.NetworkDetails["outhaul"] = docker.NetworkInfo{
		Subnet:  "172.30.0.0/24",
		Gateway: "172.30.0.1",
		IPRange: "172.30.0.128/25",
		IPv4s:   map[string]string{"other": "172.30.0.2"},
	}

	err := EnsureProxy(ctx, f, pc, nil)
	if err == nil || !strings.Contains(err.Error(), "172.30.0.2") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("EnsureProxy = %v, want an error naming the address and its holder", err)
	}
	if len(f.Created) != 0 {
		t.Errorf("containers created = %+v, want none", f.Created)
	}
	if len(f.Pulled) != 0 {
		t.Errorf("images pulled = %v, want the address checked before any pull", f.Pulled)
	}
}

// Docker can report an attached container that has no address on the network
// yet. That must not count as holding Traefik's address.
func TestEnsureProxyIgnoresAContainerWithNoAddress(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"
	f.NetworkDetails["outhaul"] = docker.NetworkInfo{
		Subnet:  "172.30.0.0/24",
		Gateway: "172.30.0.1",
		IPRange: "172.30.0.128/25",
		IPv4s:   map[string]string{"starting": ""},
	}

	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy: %v", err)
	}
	if len(f.Created) != 1 || f.Created[0].IPv4 != "172.30.0.2" {
		t.Fatalf("created specs = %+v, want one with IPv4 %q", f.Created, "172.30.0.2")
	}
}

func TestEnsureProxyKeepsTheOldTraefikWhenTheAddressIsHeld(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet = "172.30.0.0/24"
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}

	info := f.NetworkDetails["outhaul"]
	info.IPv4s["other"] = "172.30.0.2"
	f.NetworkDetails["outhaul"] = info

	pc.IPv4 = "172.30.0.2"
	if err := EnsureProxy(ctx, f, pc, nil); err == nil {
		t.Fatal("EnsureProxy with a held address: want an error")
	}
	old, _ := f.FindContainer(ctx, pc.ContainerName)
	if old == nil || !old.Running() {
		t.Errorf("the old Traefik was stopped although the address is held: %+v", old)
	}
	if len(f.Created) != 1 {
		t.Errorf("containers created = %+v, want just the original Traefik", f.Created)
	}
}

// Recreating Traefik is not a conflict with itself: the address the old
// container holds under its own name belongs to the replacement.
func TestEnsureProxyKeepsItsOwnAddress(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}

	pc.Image = "traefik:v3.4" // a change that needs a recreate
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy with its own address still attached: %v", err)
	}
	if len(f.Created) != 2 || f.Created[1].IPv4 != "172.30.0.2" {
		t.Fatalf("created specs = %+v, want the replacement with IPv4 %q", f.Created, "172.30.0.2")
	}
}

// The upgrade this setting exists for: an install that already fixed the
// subnet, with Traefik at an address Docker picked, then sets
// OUTHAUL_TRAEFIK_IP. The address is part of the container, so Traefik is
// recreated at it.
func TestEnsureProxyAddsTheAddressOnUpgrade(t *testing.T) {
	ctx := context.Background()
	f := docker.NewFake()
	pc := testProxyConfig()
	pc.Subnet = "172.30.0.0/24"
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("initial EnsureProxy: %v", err)
	}
	before, _ := f.FindContainer(ctx, pc.ContainerName)
	if before == nil {
		t.Fatal("no Traefik after the first EnsureProxy")
	}
	if len(f.Created) != 1 || f.Created[0].IPv4 != "" {
		t.Fatalf("created specs = %+v, want one with no address", f.Created)
	}

	pc.IPv4 = "172.30.0.2"
	if err := EnsureProxy(ctx, f, pc, nil); err != nil {
		t.Fatalf("EnsureProxy after setting the address: %v", err)
	}
	after, _ := f.FindContainer(ctx, pc.ContainerName)
	if after == nil || !after.Running() {
		t.Fatalf("Traefik is not running after the upgrade: %+v", after)
	}
	if after.ID == before.ID {
		t.Error("adding the address should recreate Traefik")
	}
	if len(f.Created) != 2 || f.Created[1].IPv4 != "172.30.0.2" {
		t.Fatalf("created specs = %+v, want the replacement with IPv4 %q", f.Created, "172.30.0.2")
	}
}

// inspectFails is a Docker whose network inspection fails.
type inspectFails struct{ *docker.Fake }

func (inspectFails) InspectNetwork(context.Context, string) (docker.NetworkInfo, error) {
	return docker.NetworkInfo{}, errors.New("docker unreachable")
}

// When the address can't be checked, say so: the operator needs to know the
// old Traefik was left running because the conflict could not be ruled out.
func TestEnsureProxyReportsAFailedAddressCheck(t *testing.T) {
	ctx := context.Background()
	pc := testProxyConfig()
	pc.Subnet, pc.IPv4 = "172.30.0.0/24", "172.30.0.2"
	f := &inspectFails{Fake: docker.NewFake()}

	err := EnsureProxy(ctx, f, pc, nil)
	if err == nil || !strings.Contains(err.Error(), "check who holds Traefik's address 172.30.0.2") || !strings.Contains(err.Error(), "docker unreachable") {
		t.Fatalf("EnsureProxy = %v, want the failed address check and its cause", err)
	}
	if len(f.Created) != 0 {
		t.Errorf("containers created = %+v, want none", f.Created)
	}
}
