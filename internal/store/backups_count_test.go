package store

import (
	"context"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func TestCountRunningBackupRuns(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	dest := mustDestination(t, s, "minio")
	db := mustDatabase(t, s, "shop")
	b := mustBackup(t, s, core.BackupTargetDatabase, db.ID, dest.ID)

	if _, err := s.StartBackupRun(ctx, b.ID, core.RunKindBackup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBackupRun(ctx, b.ID, core.RunKindRestore); err != nil {
		t.Fatal(err)
	}
	done, err := s.StartBackupRun(ctx, b.ID, core.RunKindBackup)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackupRun(ctx, done, core.RunOK, "", "k", 1); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountRunningBackupRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountRunningBackupRuns = %d, want 2", n)
	}
}
