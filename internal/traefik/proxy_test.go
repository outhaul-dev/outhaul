package traefik

import (
	"context"
	"strings"
	"testing"

	"github.com/slipwaydev/slipway/internal/docker"
)

func testProxyConfig() ProxyConfig {
	return ProxyConfig{
		ContainerName: "slipway-traefik",
		Image:         "traefik:v3.3",
		Network:       "slipway",
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

// recordingFake wraps the Fake to capture the ContainerSpec passed to Create.
type recordingFake struct {
	*docker.Fake
	created docker.ContainerSpec
}

func (r *recordingFake) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	r.created = spec
	return r.Fake.CreateContainer(ctx, spec)
}
