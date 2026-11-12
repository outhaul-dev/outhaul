package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/secret"
)

func TestCheckDatabaseIdle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabaseIdle(path); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("CheckDatabaseIdle with an open store = %v, want ErrDatabaseInUse", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabaseIdle(path); err != nil {
		t.Fatalf("CheckDatabaseIdle after close = %v, want nil", err)
	}
	if err := CheckDatabaseIdle(filepath.Join(t.TempDir(), "missing.db")); err != nil {
		t.Fatalf("CheckDatabaseIdle on a missing file = %v, want nil", err)
	}
}

func TestHasUserInFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.db")
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	if has, err := HasUserInFile(path); err != nil || has {
		t.Fatalf("empty store: HasUserInFile = %v, %v", has, err)
	}
	if _, err := s.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if has, err := HasUserInFile(path); err != nil || !has {
		t.Fatalf("with a user: HasUserInFile = %v, %v", has, err)
	}

	text := filepath.Join(t.TempDir(), "text.db")
	if err := os.WriteFile(text, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := HasUserInFile(text); err == nil {
		t.Error("HasUserInFile on a text file succeeded, want an error")
	}

	other := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(other))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := HasUserInFile(other); err == nil {
		t.Error("HasUserInFile on a SQLite file without users succeeded, want an error")
	}
}

func TestCheckStateCopy(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if _, err := s.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.db")
	if err := s.Snapshot(ctx, good); err != nil {
		t.Fatal(err)
	}
	names, err := CheckStateCopy(good)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(applied) || names[len(names)-1] != applied[len(applied)-1] {
		t.Errorf("CheckStateCopy names = %v, want %v", names, applied)
	}

	text := filepath.Join(dir, "text.db")
	if err := os.WriteFile(text, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckStateCopy(text); !errors.Is(err, ErrNotStateCopy) {
		t.Fatalf("CheckStateCopy(text) = %v, want ErrNotStateCopy", err)
	}

	noSchema := filepath.Join(dir, "noschema.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(noSchema))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := CheckStateCopy(noSchema); !errors.Is(err, ErrNotStateCopy) {
		t.Fatalf("CheckStateCopy(no schema) = %v, want ErrNotStateCopy", err)
	}

	newer := filepath.Join(dir, "newer.db")
	if err := copyFileForTest(good, newer); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", "file:"+url.PathEscape(newer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('migrations/9999_future.sql', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := CheckStateCopy(newer); !errors.Is(err, ErrNewerStateCopy) || !strings.Contains(err.Error(), "9999_future.sql") {
		t.Fatalf("CheckStateCopy(newer) = %v, want ErrNewerStateCopy naming the migration", err)
	}

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := copyFileForTest(good, corrupt); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the second page's b-tree header. Flipping bytes in a page's
	// payload can leave a structurally valid database, so integrity_check
	// would not notice; the page type byte is structural.
	const page = 4096
	if len(data) < page+64 {
		t.Fatalf("snapshot is %d bytes, too short for the corruption check", len(data))
	}
	for i := page; i < page+64; i++ {
		data[i] ^= 0xFF
	}
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckStateCopy(corrupt); err == nil || !strings.Contains(strings.ToLower(err.Error()), "integrity") {
		t.Fatalf("CheckStateCopy(corrupt) = %v, want an integrity error", err)
	}
}

func TestAppliedMigrationsAndStateCounts(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if _, err := s.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx, core.App{Name: "web", RepoURL: "https://example.com/r.git", Domain: "web.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDatabase(ctx, core.Database{Name: "db", Engine: core.EnginePostgres, Image: "img", Username: "u", Password: "pw", DBName: "db"}); err != nil {
		t.Fatal(err)
	}
	accounts, apps, databases, err := s.StateCounts(ctx)
	if err != nil || accounts != 1 || apps != 1 || databases != 1 {
		t.Fatalf("StateCounts = %d, %d, %d, %v", accounts, apps, databases, err)
	}
	applied, err := s.AppliedMigrations(ctx)
	if err != nil || len(applied) == 0 || applied[len(applied)-1] != "migrations/0032_app_limits.sql" {
		t.Fatalf("AppliedMigrations = %v, %v", applied, err)
	}
}

func TestRestoreMarkerRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openWithBox(t)
	if _, ok, err := s.RestoreMarker(ctx); ok || err != nil {
		t.Fatalf("absent marker = ok %v, err %v", ok, err)
	}
	want := core.RestoreMarker{
		RestoredAt: time.Date(2026, 11, 11, 2, 15, 0, 0, time.UTC),
		From:       "latest", Pending: true, Databases: []string{"db1"},
	}
	if err := s.SetRestoreMarker(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.RestoreMarker(ctx)
	if err != nil || !ok {
		t.Fatalf("RestoreMarker = ok %v, err %v", ok, err)
	}
	if !got.RestoredAt.Equal(want.RestoredAt) || got.From != want.From || got.Pending != want.Pending ||
		len(got.Databases) != 1 || got.Databases[0] != "db1" {
		t.Errorf("marker = %+v, want %+v", got, want)
	}
	if err := s.ClearRestoreMarker(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.RestoreMarker(ctx); ok || err != nil {
		t.Fatalf("cleared marker = ok %v, err %v", ok, err)
	}
}

func copyFileForTest(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
