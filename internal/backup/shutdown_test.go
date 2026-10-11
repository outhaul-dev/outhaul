package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// hangUntilCancelled makes every dump hang until its context ends, and
// returns a channel closed once a dump has started.
func (h *harness) hangUntilCancelled() <-chan struct{} {
	started := make(chan struct{})
	var once bool
	h.docker.OnExecCtx = func(ctx context.Context, _ string, _, _ []string) (string, string, int, error) {
		if !once {
			once = true
			close(started)
		}
		<-ctx.Done()
		return "", "", 0, ctx.Err()
	}
	return started
}

// Backups ran on context.Background and nothing waited for them: SIGTERM
// mid-run left the run row "running" forever (and a restore's stopped
// containers stopped). Stop cancels in-flight runs and waits for each to
// record its outcome.
func TestStopCancelsInFlightRunAndRecordsIt(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	started := h.hangUntilCancelled()

	h.m.RunNow(b)
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed || !strings.Contains(run.Reason, "interrupted") {
		t.Fatalf("run = %s %q, want failed and interrupted", run.Status, run.Reason)
	}
}

func TestStopRefusesNewRuns(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)

	if err := h.m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.m.RunNow(b)
	select {
	case <-h.m.runDone:
		t.Fatal("a backup ran after Stop")
	case <-time.After(100 * time.Millisecond):
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), b.ID, 5); len(runs) != 0 {
		t.Errorf("runs after Stop = %d, want 0", len(runs))
	}
}

// When the run timeout fired, the outcome was written with the expired
// context, the write failed, and the run stayed "running" forever.
func TestTimedOutRunIsRecordedAsFailed(t *testing.T) {
	h := newHarness(t)
	h.m.timeout = 50 * time.Millisecond
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	h.hangUntilCancelled()

	h.m.RunNow(b)
	h.waitRun(t)

	run := h.lastRun(t, b.ID)
	if run.Status != core.RunFailed || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("run = %s %q, want failed and timed out", run.Status, run.Reason)
	}
}

// A hard kill leaves rows "running" with no goroutine behind them. Boot
// recovery fails them, and a restore's reason says what may be left behind.
func TestRecoverInterruptedFailsRunningRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b := h.backup(t, core.BackupTargetDatabase, db.ID, dest.ID, 0)
	if _, err := h.store.StartBackupRun(ctx, b.ID, core.RunKindBackup); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.StartBackupRun(ctx, b.ID, core.RunKindRestore); err != nil {
		t.Fatal(err)
	}

	n, err := h.m.RecoverInterrupted(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RecoverInterrupted = %d, %v; want 2 runs", n, err)
	}
	runs, err := h.store.ListBackupRuns(ctx, b.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Status != core.RunFailed || r.FinishedAt == nil {
			t.Errorf("%s run = %s (finished %v), want failed with a finish time", r.Kind, r.Status, r.FinishedAt)
		}
		if r.Kind == core.RunKindRestore && !strings.Contains(r.Reason, "stopped") {
			t.Errorf("restore reason = %q, want a warning that containers may be stopped", r.Reason)
		}
	}
}
