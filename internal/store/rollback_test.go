package store

import (
	"context"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func TestCreateRollbackPresetsImage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")

	d, err := s.CreateRollback(ctx, app.ID, "outhaul/web:7", 7)
	if err != nil {
		t.Fatalf("CreateRollback: %v", err)
	}
	if d.Status != core.StatusQueued {
		t.Errorf("status = %q, want queued", d.Status)
	}

	// The pre-set image and provenance must survive the DB round-trip — the
	// image is what tells the pipeline to skip the build.
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Image != "outhaul/web:7" || got.RollbackOf != 7 {
		t.Errorf("round-trip = image %q rollback_of %d, want outhaul/web:7 / 7", got.Image, got.RollbackOf)
	}

	// A rollback is an ordinary queued attempt: the dispatcher must see it.
	next, err := s.NextClaimable(ctx)
	if err != nil {
		t.Fatalf("NextClaimable: %v", err)
	}
	if next == nil || next.ID != d.ID {
		t.Errorf("NextClaimable = %+v, want the rollback row", next)
	}
	if next.Image != "outhaul/web:7" {
		t.Errorf("claimable image = %q, want the pre-set tag", next.Image)
	}
}

func TestCreateDeploymentHasNoRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")

	d, err := s.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Image != "" || got.RollbackOf != 0 {
		t.Errorf("normal deploy = image %q rollback_of %d, want empty / 0", got.Image, got.RollbackOf)
	}
}
