package backup

import (
	"context"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestHeldSchedulerSkipsTheMinute(t *testing.T) {
	h := newHarness(t)
	dest := h.destination(t)
	db := h.seedRunningDatabase(t, "shop", core.EnginePostgres)
	b, err := h.store.CreateBackup(context.Background(), core.Backup{
		TargetKind: core.BackupTargetDatabase, TargetID: db.ID, DestinationID: dest.ID,
		Schedule: "* * * * *", Prefix: "prod",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.docker.OnExec = func(string, []string, []string) (string, string, int, error) { return "x", "", 0, nil }
	held := true
	h.m.SetHold(func() bool { return held })
	now := time.Date(2026, time.July, 4, 3, 30, 0, 0, time.Local)

	h.m.Tick(context.Background(), now)

	select {
	case <-h.m.runDone:
		t.Fatal("a run started while held")
	case <-time.After(100 * time.Millisecond):
	}
	if runs, _ := h.store.ListBackupRuns(context.Background(), b.ID, 5); len(runs) != 0 {
		t.Fatalf("held: %d run(s) recorded, want none", len(runs))
	}

	held = false
	h.m.Tick(context.Background(), now.Add(time.Minute))
	h.waitRun(t)
	if run := h.lastRun(t, b.ID); run.Status != core.RunOK {
		t.Errorf("released: run = %q (%s)", run.Status, run.Reason)
	}
}
