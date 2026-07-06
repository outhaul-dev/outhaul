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
