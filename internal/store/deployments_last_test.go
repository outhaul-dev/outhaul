package store

import (
	"context"
	"testing"
	"time"
)

func TestLastDeploymentAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	app := mustApp(t, s, "web")

	// No deployments yet -> ok=false.
	if _, ok, err := s.LastDeploymentAt(ctx, app.ID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("LastDeploymentAt ok=true with no deployments, want false")
	}

	before := time.Now().Add(-time.Minute)
	if _, err := s.CreateDeployment(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Minute)

	last, ok, err := s.LastDeploymentAt(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LastDeploymentAt ok=false after a deployment, want true")
	}
	if last.Before(before) || last.After(after) {
		t.Errorf("LastDeploymentAt = %v, want within [%v, %v]", last, before, after)
	}
}
