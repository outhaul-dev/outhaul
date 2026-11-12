package docker

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestFakeContainerLifecycle(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if c, _ := f.FindContainer(ctx, "web"); c != nil {
		t.Fatal("expected no container before create")
	}

	id, err := f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "web:1"})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	c, _ := f.FindContainer(ctx, "web")
	if c == nil || c.State != "created" {
		t.Fatalf("after create: %+v", c)
	}

	if err := f.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if c, _ := f.FindContainer(ctx, "web"); !c.Running() {
		t.Fatal("container should be running after start")
	}

	if err := f.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if err := f.RemoveContainer(ctx, id, true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if c, _ := f.FindContainer(ctx, "web"); c != nil {
		t.Fatal("container should be gone after remove")
	}
}

func TestFakeRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if _, err := f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "x"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "y"}); err == nil {
		t.Fatal("expected duplicate-name error, mirroring Docker")
	}
}

func TestFakeContainerIP(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.CreateContainer(ctx, ContainerSpec{Name: "c1", Image: "img"})
	ip, err := f.ContainerIP(ctx, id, "outhaul")
	if err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	if ip == "" {
		t.Error("expected a non-empty IP")
	}
	if _, err := f.ContainerIP(ctx, "missing", "outhaul"); err == nil {
		t.Error("expected error for unknown container")
	}
}

func TestFakeContainerLogs(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	id, _ := f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "img"})
	f.Logs[id] = "hello\nworld\n"

	rc, err := f.ContainerLogs(ctx, id, 500)
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello\nworld\n" {
		t.Errorf("logs = %q", b)
	}
	if len(f.LogTails) != 1 || f.LogTails[0] != 500 {
		t.Errorf("LogTails = %v, want [500]", f.LogTails)
	}
	if _, err := f.ContainerLogs(ctx, "missing", 100); err == nil {
		t.Error("expected error for unknown container, mirroring the daemon")
	}
}

func TestFakeFindReturnsCopy(t *testing.T) {
	// Callers must not be able to mutate Fake's internal state via the returned
	// container; that would hide bugs where production code relies on re-reading.
	ctx := context.Background()
	f := NewFake()
	f.CreateContainer(ctx, ContainerSpec{Name: "web", Image: "x"})
	c, _ := f.FindContainer(ctx, "web")
	c.State = "tampered"
	again, _ := f.FindContainer(ctx, "web")
	if again.State == "tampered" {
		t.Fatal("FindContainer returned a reference into internal state")
	}
}

func TestFakeCreateAndListVolumesFull(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := f.CreateVolume(ctx, "outhaul-web-data",
		map[string]string{"outhaul.managed": "true", "outhaul.role": "data", "outhaul.app": "web"}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// Idempotent: creating again with the same name is not an error.
	if err := f.CreateVolume(ctx, "outhaul-web-data", nil); err != nil {
		t.Fatalf("CreateVolume idempotent: %v", err)
	}
	got, err := f.ListVolumesFull(ctx, map[string]string{"outhaul.role": "data"})
	if err != nil {
		t.Fatalf("ListVolumesFull: %v", err)
	}
	if len(got) != 1 || got[0].Name != "outhaul-web-data" || got[0].Labels["outhaul.app"] != "web" {
		t.Fatalf("ListVolumesFull = %+v", got)
	}
}

func TestFakeListVolumesFullLabelPresence(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Volumes["proj_data"] = map[string]string{"com.docker.compose.project": "outhaul-shop"}
	f.Volumes["loose"] = map[string]string{"other": "x"}
	// An empty match value means "label present with any value".
	got, err := f.ListVolumesFull(ctx, map[string]string{"com.docker.compose.project": ""})
	if err != nil {
		t.Fatalf("ListVolumesFull: %v", err)
	}
	if len(got) != 1 || got[0].Name != "proj_data" {
		t.Fatalf("presence match = %+v, want just proj_data", got)
	}
}

func TestFakeRemoveVolume(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Volumes["v1"] = map[string]string{"outhaul.managed": "true"}
	if err := f.RemoveVolume(ctx, "v1", false); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	if _, ok := f.Volumes["v1"]; ok {
		t.Fatal("volume still present after RemoveVolume")
	}
	// Not-found is success per the Client contract.
	if err := f.RemoveVolume(ctx, "does-not-exist", false); err != nil {
		t.Fatalf("RemoveVolume of a missing volume should be nil, got %v", err)
	}
}

// A fixed address belongs to the container that holds it. Removing or renaming
// that container must move the bookkeeping with it, or a fake-backed test
// reports a conflict the real daemon does not.
func TestFakeTracksRemovedAndRenamedAddresses(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := f.EnsureNetwork(ctx, NetworkSpec{
		Name: "outhaul", Subnet: "172.30.0.0/24", Gateway: "172.30.0.1", IPRange: "172.30.0.128/25",
	}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	id, err := f.CreateContainer(ctx, ContainerSpec{
		Name: "outhaul-traefik", Image: "traefik:example", Networks: []string{"outhaul"}, IPv4: "172.30.0.2",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got := f.NetworkDetails["outhaul"].IPv4s["outhaul-traefik"]; got != "172.30.0.2" {
		t.Fatalf("IPv4s[outhaul-traefik] = %q, want %q", got, "172.30.0.2")
	}

	if err := f.RenameContainer(ctx, id, "outhaul-traefik-old"); err != nil {
		t.Fatalf("RenameContainer: %v", err)
	}
	addresses := f.NetworkDetails["outhaul"].IPv4s
	if _, stale := addresses["outhaul-traefik"]; stale {
		t.Errorf("IPv4s still names the old container: %v", addresses)
	}
	if got := addresses["outhaul-traefik-old"]; got != "172.30.0.2" {
		t.Errorf("IPv4s[outhaul-traefik-old] = %q, want %q", got, "172.30.0.2")
	}

	if err := f.RemoveContainer(ctx, id, true); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if addresses := f.NetworkDetails["outhaul"].IPv4s; len(addresses) != 0 {
		t.Errorf("IPv4s = %v, want the removed container's address gone", addresses)
	}
}

// The real client always returns an address map; a caller may write into it.
// A network the fake only knows by name must not yield a nil map.
func TestFakeInspectNetworkAlwaysHasTheAddressMap(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Networks["outhaul"] = true

	info, err := f.InspectNetwork(ctx, "outhaul")
	if err != nil {
		t.Fatalf("InspectNetwork: %v", err)
	}
	if info.IPv4s == nil {
		t.Fatal("InspectNetwork returned a nil IPv4s map for a known network")
	}
	info.IPv4s["probe"] = "172.30.0.9"

	if err := f.EnsureNetwork(ctx, NetworkSpec{Name: "outhaul"}); err != nil {
		t.Fatalf("EnsureNetwork for an existing name: %v", err)
	}
	info, err = f.InspectNetwork(ctx, "outhaul")
	if err != nil {
		t.Fatalf("InspectNetwork after EnsureNetwork: %v", err)
	}
	if info.IPv4s == nil {
		t.Fatal("EnsureNetwork left the address map nil")
	}
}
