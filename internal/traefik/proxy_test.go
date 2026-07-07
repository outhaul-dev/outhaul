package traefik

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/docker"
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
	pc.TLSEnabled = true
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
	pc.TLSEnabled = false // a cert is impossible, so no admin route
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
	pc := ProxyConfig{TLSEnabled: true, DynamicDir: dir}
	if err := writeAdminDynamicConfig(pc); err != nil {
		t.Fatalf("writeAdminDynamicConfig: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale dynamic config should have been removed (err=%v)", err)
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
