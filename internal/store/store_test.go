package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustApp(t *testing.T, s *Store, name string) core.App {
	t.Helper()
	app, err := s.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/r.git", Domain: name + ".test",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func TestMigrationsRunOnOpen(t *testing.T) {
	s := newTestStore(t)
	// A working query against a migrated table proves migrations ran.
	if _, err := s.ListApps(context.Background()); err != nil {
		t.Fatalf("ListApps after Open: %v", err)
	}
}

func TestCreateAndGetApp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	app := mustApp(t, s, "web")
	if app.ID == 0 {
		t.Fatal("CreateApp did not assign an ID")
	}
	if app.CreatedAt.IsZero() {
		t.Error("CreateApp did not set CreatedAt")
	}

	got, err := s.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Name != "web" || got.Domain != "web.test" {
		t.Errorf("GetApp = %+v", got)
	}
}

func TestCreateAppRejectsDuplicateName(t *testing.T) {
	s := newTestStore(t)
	mustApp(t, s, "web")
	_, err := s.CreateApp(context.Background(), core.App{Name: "web", RepoURL: "x", Domain: "y"})
	if err == nil {
		t.Fatal("expected error creating app with duplicate name")
	}
}

func TestCreateDeploymentStartsQueued(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")

	d, err := s.CreateDeployment(ctx, app.ID)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if d.Status != core.StatusQueued {
		t.Errorf("new deployment status = %q, want queued", d.Status)
	}
	if d.StartedAt != nil || d.FinishedAt != nil {
		t.Error("new deployment should have nil started/finished timestamps")
	}
}

func TestClaimDeploymentIsAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")
	d, _ := s.CreateDeployment(ctx, app.ID)

	ok, err := s.ClaimDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("ClaimDeployment: %v", err)
	}
	if !ok {
		t.Fatal("first claim should succeed")
	}

	// Second claim of the same (now building) deployment must not win.
	ok, err = s.ClaimDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("ClaimDeployment (2nd): %v", err)
	}
	if ok {
		t.Fatal("second claim should fail: already building")
	}

	got, _ := s.GetDeployment(ctx, d.ID)
	if got.Status != core.StatusBuilding {
		t.Errorf("status after claim = %q, want building", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("claim should set StartedAt")
	}
}

func TestSetStatusEnforcesStateMachine(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")
	d, _ := s.CreateDeployment(ctx, app.ID)
	s.ClaimDeployment(ctx, d.ID) // -> building

	// Illegal: building -> running (must go via deploying).
	ok, err := s.SetStatus(ctx, d.ID, core.StatusBuilding, core.StatusRunning, "")
	if err != nil {
		t.Fatalf("SetStatus illegal: %v", err)
	}
	if ok {
		t.Fatal("building -> running should be rejected by the state machine")
	}

	// Legal: building -> deploying.
	ok, err = s.SetStatus(ctx, d.ID, core.StatusBuilding, core.StatusDeploying, "")
	if err != nil {
		t.Fatalf("SetStatus legal: %v", err)
	}
	if !ok {
		t.Fatal("building -> deploying should succeed")
	}
}

func TestSetStatusGuardsAgainstStaleFrom(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")
	d, _ := s.CreateDeployment(ctx, app.ID)
	s.ClaimDeployment(ctx, d.ID) // now building, not queued

	// Caller believes it is still queued: guarded update must not apply.
	ok, err := s.SetStatus(ctx, d.ID, core.StatusQueued, core.StatusCancelled, "")
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if ok {
		t.Fatal("update with stale 'from' status should not apply")
	}
}

func TestSetStatusTerminalSetsFinishedAndReason(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	app := mustApp(t, s, "web")
	d, _ := s.CreateDeployment(ctx, app.ID)
	s.ClaimDeployment(ctx, d.ID)

	ok, err := s.SetStatus(ctx, d.ID, core.StatusBuilding, core.StatusFailed, "clone failed")
	if err != nil || !ok {
		t.Fatalf("SetStatus failed: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetDeployment(ctx, d.ID)
	if got.Reason != "clone failed" {
		t.Errorf("reason = %q", got.Reason)
	}
	if got.FinishedAt == nil {
		t.Error("terminal status should set FinishedAt")
	}
}

func TestNextClaimableSerializesPerApp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	web := mustApp(t, s, "web")
	api := mustApp(t, s, "api")

	// web has two queued deployments; api has one.
	d1, _ := s.CreateDeployment(ctx, web.ID)
	_, _ = s.CreateDeployment(ctx, web.ID) // d2, same app
	dapi, _ := s.CreateDeployment(ctx, api.ID)

	// First claimable: oldest queued overall (d1).
	got, err := s.NextClaimable(ctx)
	if err != nil {
		t.Fatalf("NextClaimable: %v", err)
	}
	if got == nil || got.ID != d1.ID {
		t.Fatalf("NextClaimable = %v, want d1 (%d)", got, d1.ID)
	}

	// Simulate the worker claiming d1: web now has an active deployment.
	s.ClaimDeployment(ctx, d1.ID)

	// Next claimable must skip web's d2 (app busy) and return api's deployment.
	got, err = s.NextClaimable(ctx)
	if err != nil {
		t.Fatalf("NextClaimable (2): %v", err)
	}
	if got == nil || got.ID != dapi.ID {
		t.Fatalf("NextClaimable = %v, want api deployment (%d)", got, dapi.ID)
	}
}

func TestNextClaimableEmptyWhenAllBusyOrDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	web := mustApp(t, s, "web")
	d, _ := s.CreateDeployment(ctx, web.ID)
	s.ClaimDeployment(ctx, d.ID) // building; web busy, no other queued

	got, err := s.NextClaimable(ctx)
	if err != nil {
		t.Fatalf("NextClaimable: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no claimable deployment, got %v", got)
	}
}

func TestRecoverActiveFailsInFlightOnBoot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	web := mustApp(t, s, "web")

	building, _ := s.CreateDeployment(ctx, web.ID)
	s.ClaimDeployment(ctx, building.ID) // building

	deploying, _ := s.CreateDeployment(ctx, mustApp(t, s, "api").ID)
	s.ClaimDeployment(ctx, deploying.ID)
	s.SetStatus(ctx, deploying.ID, core.StatusBuilding, core.StatusDeploying, "")

	queued, _ := s.CreateDeployment(ctx, mustApp(t, s, "db").ID) // stays queued

	n, err := s.RecoverActive(ctx, "interrupted by restart")
	if err != nil {
		t.Fatalf("RecoverActive: %v", err)
	}
	if n != 2 {
		t.Errorf("RecoverActive marked %d, want 2 (building+deploying)", n)
	}

	for _, id := range []int64{building.ID, deploying.ID} {
		got, _ := s.GetDeployment(ctx, id)
		if got.Status != core.StatusFailed {
			t.Errorf("deployment %d status = %q, want failed", id, got.Status)
		}
		if got.Reason != "interrupted by restart" {
			t.Errorf("deployment %d reason = %q", id, got.Reason)
		}
	}
	got, _ := s.GetDeployment(ctx, queued.ID)
	if got.Status != core.StatusQueued {
		t.Errorf("queued deployment should be untouched, got %q", got.Status)
	}
}

func TestUsersAndSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	has, err := s.HasUser(ctx)
	if err != nil {
		t.Fatalf("HasUser: %v", err)
	}
	if has {
		t.Fatal("fresh store should have no user")
	}

	u, err := s.CreateUser(ctx, "admin", "hash123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if has, _ := s.HasUser(ctx); !has {
		t.Fatal("HasUser should be true after CreateUser")
	}

	got, err := s.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByName: %v", err)
	}
	if got.PasswordHash != "hash123" {
		t.Errorf("hash = %q", got.PasswordHash)
	}

	exp := time.Now().Add(time.Hour)
	if err := s.CreateSession(ctx, core.Session{Token: "tok", UserID: u.ID, ExpiresAt: exp}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := s.GetSession(ctx, "tok")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.UserID != u.ID {
		t.Errorf("session user = %d, want %d", sess.UserID, u.ID)
	}

	if err := s.DeleteSession(ctx, "tok"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "tok"); err == nil {
		t.Fatal("GetSession should fail after delete")
	}
}
