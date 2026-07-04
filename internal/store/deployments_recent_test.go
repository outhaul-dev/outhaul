package store

import (
	"context"
	"testing"
)

func TestListRecentDeploymentsAcrossApps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a1 := mustApp(t, s, "web")
	a2 := mustApp(t, s, "api")

	if _, err := s.CreateDeployment(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment(ctx, a2.ID); err != nil {
		t.Fatal(err)
	}
	last, err := s.CreateDeployment(ctx, a2.ID)
	if err != nil {
		t.Fatal(err)
	}

	recent, err := s.ListRecentDeployments(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("len = %d, want 2 (limited)", len(recent))
	}
	if recent[0].ID != last.ID {
		t.Errorf("recent[0].ID = %d, want %d (newest first)", recent[0].ID, last.ID)
	}

	n, err := s.CountDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("CountDeployments = %d, want 3", n)
	}
}
