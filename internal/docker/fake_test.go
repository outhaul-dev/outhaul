package docker

import (
	"context"
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
