package githook

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/james-smart/outhaul/internal/core"
	"github.com/james-smart/outhaul/internal/gitrepo"
	"github.com/james-smart/outhaul/internal/logstream"
	"github.com/james-smart/outhaul/internal/store"
)

func TestRequestRoundTrip(t *testing.T) {
	req := request{App: "api", Refs: []RefUpdate{
		{"000", "abc", "refs/heads/main"},
		{"111", "def", "refs/heads/dev"},
	}}
	var buf strings.Builder
	if err := writeRequest(&buf, req); err != nil {
		t.Fatalf("writeRequest: %v", err)
	}
	got, err := readRequest(newBufReader(buf.String()))
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if got.App != "api" || len(got.Refs) != 2 || got.Refs[1].Ref != "refs/heads/dev" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

type fakeNotifier struct{}

func (fakeNotifier) Notify() {}

func newBridgeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func bridgeEnv(t *testing.T) (*Bridge, *store.Store, *logstream.Broker) {
	t.Helper()
	st := newBridgeStore(t)
	br := logstream.New()
	repos := gitrepo.New(t.TempDir(), "/bin/outhaul", "/tmp/s.sock")
	b := NewBridge(st, br, fakeNotifier{}, repos, "203.0.113.9", 30*time.Second)
	return b, st, br
}

func TestBridgeDeploysDeployBranchAndGatesSuccess(t *testing.T) {
	b, st, br := bridgeEnv(t)
	ctx := context.Background()

	app, err := st.CreateApp(ctx, core.App{Name: "api", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dep := waitForDeployment(t, st, app.ID)
		br.Publish(dep.ID, "→ building image")
		markTerminal(t, st, dep, core.StatusRunning)
		br.Publish(dep.ID, "→ health OK")
		br.Close(dep.ID)
	}()

	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc", "refs/heads/main"}}})
	wg.Wait()

	if !strings.Contains(out, "→ building image") || !strings.Contains(out, "✓ deployed") {
		t.Fatalf("missing streamed/success lines:\n%s", out)
	}
	if !strings.Contains(out, "exit 0") {
		t.Fatalf("expected exit 0:\n%s", out)
	}
}

func TestBridgeNonDeployBranchDoesNotDeploy(t *testing.T) {
	b, st, _ := bridgeEnv(t)
	if _, err := st.CreateApp(context.Background(), core.App{Name: "api", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc", "refs/heads/dev"}}})
	if !strings.Contains(out, "no deploy") || !strings.Contains(out, "exit 0") {
		t.Fatalf("expected stored-not-deployed, exit 0:\n%s", out)
	}
}

func TestBridgeRejectsNonPushApp(t *testing.T) {
	b, st, _ := bridgeEnv(t)
	if _, err := st.CreateApp(context.Background(), core.App{Name: "api", Source: core.SourcePublic, Branch: "main", RepoURL: "https://x/y", Kind: core.KindNixpacks}); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc", "refs/heads/main"}}})
	if !strings.Contains(out, "not push-deployable") || !strings.Contains(out, "exit 1") {
		t.Fatalf("expected rejection, exit 1:\n%s", out)
	}
}

func TestBridgeFailedDeployGatesExit1(t *testing.T) {
	b, st, br := bridgeEnv(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, core.App{Name: "api", Source: core.SourcePush, Branch: "main", Kind: core.KindNixpacks})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dep := waitForDeployment(t, st, app.ID)
		br.Publish(dep.ID, "→ building image")
		markFailed(t, st, dep, "build failed")
		br.Close(dep.ID)
	}()
	out := runHandle(t, b, request{App: "api", Refs: []RefUpdate{{"0", "abc", "refs/heads/main"}}})
	wg.Wait()
	if !strings.Contains(out, "✗ deploy failed: build failed") || !strings.Contains(out, "exit 1") {
		t.Fatalf("expected failure gate:\n%s", out)
	}
}

func TestBridgeColdCreateGuards(t *testing.T) {
	b, _, _ := bridgeEnv(t)
	// No refs at all.
	out := runHandle(t, b, request{App: "brand-new", Refs: nil})
	if !strings.Contains(out, "nothing pushed") || !strings.Contains(out, "exit 1") {
		t.Fatalf("empty refs: %s", out)
	}
	// A tag ref, not a branch.
	out = runHandle(t, b, request{App: "brand-new", Refs: []RefUpdate{{"0", "abc", "refs/tags/v1"}}})
	if !strings.Contains(out, "must target a branch") || !strings.Contains(out, "exit 1") {
		t.Fatalf("tag ref: %s", out)
	}
}

// --- helpers ---

func newBufReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

// runHandle drives Bridge.Handle over an in-memory pipe and returns everything
// the bridge wrote back (progress lines + trailing "exit N").
func runHandle(t *testing.T, b *Bridge, req request) string {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { b.Handle(context.Background(), server); server.Close(); close(done) }()

	go func() {
		writeRequest(client, req)
	}()
	data, _ := io.ReadAll(client)
	client.Close()
	<-done
	return string(data)
}

func waitForDeployment(t *testing.T, st *store.Store, appID int64) core.Deployment {
	t.Helper()
	for i := 0; i < 400; i++ {
		deps, err := st.ListDeploymentsForApp(context.Background(), appID)
		if err == nil && len(deps) > 0 {
			return deps[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no deployment created for app %d", appID)
	return core.Deployment{}
}

func markTerminal(t *testing.T, st *store.Store, dep core.Deployment, final core.DeployStatus) {
	t.Helper()
	steps := []core.DeployStatus{core.StatusBuilding, core.StatusDeploying, final}
	from := core.StatusQueued
	for _, to := range steps {
		if _, err := st.SetStatus(context.Background(), dep.ID, from, to, ""); err != nil {
			t.Fatalf("SetStatus %s→%s: %v", from, to, err)
		}
		from = to
	}
}

func markFailed(t *testing.T, st *store.Store, dep core.Deployment, reason string) {
	t.Helper()
	if _, err := st.SetStatus(context.Background(), dep.ID, core.StatusQueued, core.StatusBuilding, ""); err != nil {
		t.Fatalf("SetStatus queued→building: %v", err)
	}
	if _, err := st.SetStatus(context.Background(), dep.ID, core.StatusBuilding, core.StatusFailed, reason); err != nil {
		t.Fatalf("SetStatus building→failed: %v", err)
	}
}
