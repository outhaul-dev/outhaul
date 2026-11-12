package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func queueApp(t *testing.T, s *Store, name string) core.App {
	t.Helper()
	app, err := s.CreateApp(context.Background(), core.App{Name: name, RepoURL: "https://example.com/" + name + ".git"})
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", name, err)
	}
	return app
}

// When every build slot is taken, the queue offers only a deployment that
// already has an image: a rollback runs beside builds.
func TestNextClaimablePrefersImagesWhenItCannotBuild(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	a, b, c := queueApp(t, s, "a"), queueApp(t, s, "b"), queueApp(t, s, "c")

	build, err := s.CreateDeployment(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := s.CreateRollback(ctx, b.ID, "img:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.NextClaimable(ctx, false)
	if err != nil || got == nil || got.ID != rollback.ID {
		t.Fatalf("NextClaimable(canBuild=false) = %+v, %v; want the rollback %d", got, err, rollback.ID)
	}
	got, err = s.NextClaimable(ctx, true)
	if err != nil || got == nil || got.ID != build.ID {
		t.Fatalf("NextClaimable(canBuild=true) = %+v, %v; want the oldest build %d", got, err, build.ID)
	}
}

// A rollback never jumps an older queued build of its own app: with no slot
// for that build, neither deployment is claimable.
func TestNextClaimableDoesNotSkipAnOlderQueuedBuild(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	a := queueApp(t, s, "a")

	build, err := s.CreateDeployment(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRollback(ctx, a.ID, "img:1", 0); err != nil {
		t.Fatal(err)
	}

	if got, err := s.NextClaimable(ctx, false); err != nil || got != nil {
		t.Fatalf("NextClaimable(canBuild=false) = %+v, %v; want nil while the older build waits", got, err)
	}
	if got, err := s.NextClaimable(ctx, true); err != nil || got == nil || got.ID != build.ID {
		t.Fatalf("NextClaimable(canBuild=true) = %+v, %v; want the older build %d", got, err, build.ID)
	}
}
