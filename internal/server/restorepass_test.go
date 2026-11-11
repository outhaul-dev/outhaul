package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
)

func pendingRestoreMarker(t *testing.T, e *testEnv) {
	t.Helper()
	if err := e.store.SetRestoreMarker(context.Background(), core.RestoreMarker{
		RestoredAt: time.Now().UTC(), Pending: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func seedRestoreApp(t *testing.T, e *testEnv, name, image string, ephemeral bool, source string) (core.App, core.Deployment) {
	t.Helper()
	app, err := e.store.CreateApp(context.Background(), core.App{
		Name: name, RepoURL: "https://example.com/" + name + ".git",
		Domain: name + ".example.com", Ephemeral: ephemeral, Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	var live core.Deployment
	if image != "" {
		live = seedFinishedDeploy(t, e, app.ID, image)
	}
	return app, live
}

func seedRestoreDatabase(t *testing.T, e *testEnv, name, status string) core.Database {
	t.Helper()
	db, err := e.store.CreateDatabase(context.Background(), core.Database{
		Name: name, Engine: core.EnginePostgres, Image: "img",
		Username: "u", Password: "pw", DBName: "db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetDatabaseStatus(context.Background(), db.ID, status, ""); err != nil {
		t.Fatal(err)
	}
	return db
}

func restoreMarkerOf(t *testing.T, e *testEnv) core.RestoreMarker {
	t.Helper()
	m, ok, err := e.store.RestoreMarker(context.Background())
	if err != nil || !ok {
		t.Fatalf("marker = ok %v, err %v", ok, err)
	}
	return m
}

func wantNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

func TestRestorePassOrderAndChoices(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	pendingRestoreMarker(t, e)

	seedRestoreDatabase(t, e, "db1", core.DBRunning)
	seedRestoreDatabase(t, e, "db2", core.DBStopped)
	e.runtime.byName = map[string]*docker.Container{}

	a, liveA := seedRestoreApp(t, e, "a", "outhaul/a:1", false, core.SourcePublic)
	b, _ := seedRestoreApp(t, e, "b", "outhaul/b:1", false, core.SourcePublic)
	c, _ := seedRestoreApp(t, e, "c", "outhaul/c:1", false, core.SourcePublic)
	d, _ := seedRestoreApp(t, e, "d", "", false, core.SourcePublic)
	p, _ := seedRestoreApp(t, e, "p", "outhaul/p:1", true, core.SourcePublic)
	push, _ := seedRestoreApp(t, e, "push", "outhaul/push:1", false, core.SourcePush)

	e.runtime.stack = []docker.Container{{
		ID: "c-c", Name: core.AppContainerName(c.Name), State: "running",
		Labels: map[string]string{"outhaul.app": c.Name},
	}}
	e.runtime.images = []string{"outhaul/a:1", "outhaul/p:1"}

	if err := e.srv.runRestorePassOnce(ctx, restoreMarkerOf(t, e)); err != nil {
		t.Fatal(err)
	}

	if len(e.databases.provisionedSync) != 1 || e.databases.provisionedSync[0].Name != "db1" {
		t.Errorf("provisioned = %+v, want only db1", e.databases.provisionedSync)
	}
	depsA := deploymentsOf(t, e, a.ID)
	if len(depsA) != 2 || depsA[0].Status != core.StatusQueued ||
		depsA[0].Image != "outhaul/a:1" || depsA[0].RollbackOf != liveA.ID {
		t.Errorf("a deployments = %+v, want a reuse of outhaul/a:1", depsA)
	}
	depsB := deploymentsOf(t, e, b.ID)
	if len(depsB) != 2 || depsB[0].Image != "" || depsB[0].RollbackOf != 0 {
		t.Errorf("b deployments = %+v, want a normal deploy", depsB)
	}
	if depsC := deploymentsOf(t, e, c.ID); len(depsC) != 1 {
		t.Errorf("c deployments = %+v, want only its live row", depsC)
	}
	if depsD := deploymentsOf(t, e, d.ID); len(depsD) != 0 {
		t.Errorf("d deployments = %+v, want none", depsD)
	}
	depsP := deploymentsOf(t, e, p.ID)
	if len(depsP) != 2 || depsP[0].Image != "outhaul/p:1" {
		t.Errorf("p deployments = %+v, want a reuse of outhaul/p:1", depsP)
	}
	depsPush := deploymentsOf(t, e, push.ID)
	if len(depsPush) != 1 {
		t.Errorf("push deployments = %+v, want only its live row", depsPush)
	}

	m := restoreMarkerOf(t, e)
	if m.Pending {
		t.Error("marker is still pending")
	}
	wantNames(t, "Databases", m.Databases, []string{"db1"})
	wantNames(t, "Queued", m.Queued, []string{"a", "b", "p"})
	wantNames(t, "WaitingPush", m.WaitingPush, []string{"push"})
}

func TestRestorePassRecordsFailures(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	pendingRestoreMarker(t, e)
	db1 := seedRestoreDatabase(t, e, "db1", core.DBRunning)
	e.databases.failWith = errors.New("pull failed")
	seedRestoreApp(t, e, "a", "outhaul/a:1", false, core.SourcePublic)
	b, _ := seedRestoreApp(t, e, "b", "outhaul/b:1", false, core.SourcePublic)
	e.runtime.byName = map[string]*docker.Container{}
	e.runtime.listErrOf = map[string]error{"a": errors.New("docker down")}

	if err := e.srv.runRestorePassOnce(ctx, restoreMarkerOf(t, e)); err != nil {
		t.Fatalf("the pass stopped on a per-target failure: %v", err)
	}
	db, err := e.store.GetDatabase(ctx, db1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if db.Status != core.DBFailed || db.Reason != "pull failed" {
		t.Errorf("db1 = %q %q, want failed with the reason", db.Status, db.Reason)
	}
	m := restoreMarkerOf(t, e)
	if m.Pending {
		t.Error("marker is still pending")
	}
	joined := strings.Join(m.Failed, "\n")
	for _, want := range []string{"database db1: pull failed", "app a: docker down"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Failed = %q, want %q", m.Failed, want)
		}
	}
	if deps := deploymentsOf(t, e, b.ID); len(deps) != 2 || deps[0].Image != "" {
		t.Errorf("b deployments = %+v, want a normal deploy", deps)
	}
}

func TestRestorePassDoesNothingWithoutTheMarker(t *testing.T) {
	e := newTestEnv(t)
	e.srv.restoreEvery = time.Millisecond
	e.srv.RunRestorePass(context.Background())
	if len(e.databases.provisionedSync) != 0 {
		t.Errorf("provisioned %+v, want nothing", e.databases.provisionedSync)
	}
	if e.runtime.listCalls != 0 {
		t.Errorf("listed containers %d times, want none", e.runtime.listCalls)
	}
}

func TestRestorePassRunsOnce(t *testing.T) {
	e := newTestEnv(t)
	pendingRestoreMarker(t, e)
	seedRestoreDatabase(t, e, "db1", core.DBRunning)
	e.runtime.byName = map[string]*docker.Container{}
	e.srv.restoreEvery = time.Millisecond

	e.srv.RunRestorePass(context.Background())
	if len(e.databases.provisionedSync) != 1 {
		t.Fatalf("provisioned = %+v, want one", e.databases.provisionedSync)
	}
	e.srv.RunRestorePass(context.Background())
	if len(e.databases.provisionedSync) != 1 {
		t.Errorf("provisioned = %+v after a second wait, want still one", e.databases.provisionedSync)
	}
}

// flakyRuntime fails ListImages until the test clears the error.
type flakyRuntime struct {
	*fakeRuntime
	fail  atomic.Bool
	calls atomic.Int32
}

func (f *flakyRuntime) ListImages(ctx context.Context, pattern string) ([]string, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return nil, errors.New("docker down")
	}
	return f.fakeRuntime.ListImages(ctx, pattern)
}

func TestRestorePassRetriesUntilItRuns(t *testing.T) {
	e := newTestEnv(t)
	pendingRestoreMarker(t, e)
	fr := &flakyRuntime{fakeRuntime: e.runtime}
	fr.fail.Store(true)
	e.srv.runtime = fr
	e.srv.restoreEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.srv.RunRestorePass(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for fr.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the pass ran %d time(s), want a retry", fr.calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	fr.fail.Store(false)
	for {
		m, ok, err := e.store.RestoreMarker(ctx)
		if err == nil && ok && !m.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the marker never became done")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunRestorePass did not stop")
	}
}

func TestRestoreNoticeOnSettings(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	ctx := context.Background()

	if err := e.store.SetRestoreMarker(ctx, core.RestoreMarker{
		RestoredAt: time.Now().UTC(), Pending: true,
	}); err != nil {
		t.Fatal(err)
	}
	page := body(t, e.get(t, "/settings"))
	if !strings.Contains(page, "retries each minute") {
		t.Error("the pending notice is missing its retry sentence")
	}

	if err := e.store.SetRestoreMarker(ctx, core.RestoreMarker{
		RestoredAt: time.Now().UTC(), Pending: false,
		Databases: []string{"db1"}, Queued: []string{"web"},
		WaitingPush: []string{"push"}, Failed: []string{"app x: boom"}, PrunedImages: 2,
	}); err != nil {
		t.Fatal(err)
	}
	page = body(t, e.get(t, "/settings"))
	for _, want := range []string{"Restored", "db1", "web", "push", "app x: boom", "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("the done notice lacks %q", want)
		}
	}

	resp := e.postForm(t, "/settings/restore/dismiss", nil)
	if resp.StatusCode != 303 {
		t.Fatalf("dismiss status = %d, want 303", resp.StatusCode)
	}
	if page := body(t, e.get(t, "/settings")); strings.Contains(page, `id="restore-notice"`) {
		t.Error("the notice survived Dismiss")
	}
}

func TestDismissRestoreNoticeNeedsSystemAdmin(t *testing.T) {
	e := newTestEnv(t)
	e.login(t)
	ctx := context.Background()
	if err := e.store.SetRestoreMarker(ctx, core.RestoreMarker{
		RestoredAt: time.Now().UTC(), Pending: false,
	}); err != nil {
		t.Fatal(err)
	}
	p, err := e.store.CreateProject(ctx, core.Project{Name: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	token := e.signInAs(t, "viewer", core.RoleProjectViewer, p.ID)
	resp := e.postAs(t, token, "/settings/restore/dismiss", nil)
	if resp.StatusCode != 403 {
		t.Fatalf("viewer dismiss status = %d, want 403", resp.StatusCode)
	}
	if _, ok, err := e.store.RestoreMarker(ctx); err != nil || !ok {
		t.Errorf("marker after a refused dismiss = ok %v, err %v", ok, err)
	}
}
