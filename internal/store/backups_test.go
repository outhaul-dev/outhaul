package store

import (
	"context"
	"errors"
	"testing"

	"github.com/james-smart/outhaul/internal/core"
)

func mustDestination(t *testing.T, s *Store, name string) core.Destination {
	t.Helper()
	d, err := s.CreateDestination(context.Background(), core.Destination{
		Name: name, Endpoint: "https://s3.example.com", Region: "eu-west-2",
		Bucket: "outhaul", AccessKey: "AK", SecretKey: "very-secret",
	})
	if err != nil {
		t.Fatalf("CreateDestination: %v", err)
	}
	return d
}

func mustBackup(t *testing.T, s *Store, kind string, targetID, destID int64) core.Backup {
	t.Helper()
	b, err := s.CreateBackup(context.Background(), core.Backup{
		TargetKind: kind, TargetID: targetID, DestinationID: destID,
		Schedule: "0 3 * * *", Prefix: "prod", Retention: 7,
	})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	return b
}

func TestDestinationRoundTripAndEncryption(t *testing.T) {
	s := openWithBox(t)
	d := mustDestination(t, s, "minio")

	got, err := s.GetDestination(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretKey != "very-secret" || got.Endpoint != "https://s3.example.com" ||
		got.Region != "eu-west-2" || got.Bucket != "outhaul" || got.AccessKey != "AK" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	var raw string
	if err := s.db.QueryRow(`SELECT secret_key FROM destinations WHERE id = ?`, d.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "very-secret" {
		t.Fatal("secret key stored in plaintext")
	}
}

func TestDeleteDestinationGuard(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	dest := mustDestination(t, s, "minio")
	db := mustDatabase(t, s, "shop")
	b := mustBackup(t, s, core.BackupTargetDatabase, db.ID, dest.ID)

	if err := s.DeleteDestination(ctx, dest.ID); !errors.Is(err, ErrDestinationInUse) {
		t.Fatalf("DeleteDestination with backups = %v, want ErrDestinationInUse", err)
	}
	if err := s.DeleteBackup(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, dest.ID); err != nil {
		t.Fatalf("DeleteDestination after removing backups: %v", err)
	}
}

func TestBackupCRUDAndEnabledList(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	dest := mustDestination(t, s, "minio")
	db := mustDatabase(t, s, "shop")
	b := mustBackup(t, s, core.BackupTargetDatabase, db.ID, dest.ID)

	got, err := s.GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schedule != "0 3 * * *" || got.Prefix != "prod" || got.Retention != 7 || !got.Enabled {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if err := s.SetBackupEnabled(ctx, b.ID, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.ListEnabledBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 0 {
		t.Errorf("disabled backup still in the scheduler worklist: %+v", enabled)
	}
	if list, _ := s.ListBackupsForTarget(ctx, core.BackupTargetDatabase, db.ID); len(list) != 1 {
		t.Errorf("target list = %v, want the paused backup still listed", list)
	}
}

func TestBackupRunsHistoryCapped(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	dest := mustDestination(t, s, "minio")
	db := mustDatabase(t, s, "shop")
	b := mustBackup(t, s, core.BackupTargetDatabase, db.ID, dest.ID)

	for i := 0; i < runHistoryCap+5; i++ {
		id, err := s.StartBackupRun(ctx, b.ID, core.RunKindBackup)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackupRun(ctx, id, core.RunOK, "", "k", 1); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListBackupRuns(ctx, b.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != runHistoryCap {
		t.Errorf("history = %d rows, want capped at %d", len(runs), runHistoryCap)
	}
	if runs[0].Status != core.RunOK || runs[0].ObjectKey != "k" || runs[0].FinishedAt == nil {
		t.Errorf("newest run = %+v", runs[0])
	}
}

func TestDeleteAppAndDatabaseCascadeBackups(t *testing.T) {
	s := openWithBox(t)
	ctx := context.Background()
	dest := mustDestination(t, s, "minio")

	db := mustDatabase(t, s, "shop")
	mustBackup(t, s, core.BackupTargetDatabase, db.ID, dest.ID)
	if err := s.DeleteDatabase(ctx, db.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBackupsForTarget(ctx, core.BackupTargetDatabase, db.ID); len(list) != 0 {
		t.Errorf("database backups survived database delete: %v", list)
	}

	app := mustApp(t, s, "wiki")
	mustBackup(t, s, core.BackupTargetApp, app.ID, dest.ID)
	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBackupsForTarget(ctx, core.BackupTargetApp, app.ID); len(list) != 0 {
		t.Errorf("app backups survived app delete: %v", list)
	}
	// With nothing referencing it, the destination is deletable again.
	if err := s.DeleteDestination(ctx, dest.ID); err != nil {
		t.Errorf("DeleteDestination after cascades: %v", err)
	}
}
