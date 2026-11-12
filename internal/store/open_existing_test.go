package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/outhaul-dev/outhaul/internal/core"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "outhaul.db")
}

func createCurrentDB(t *testing.T, path string) {
	t.Helper()
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func lastMigration(t *testing.T) string {
	t.Helper()
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names[len(names)-1]
}

func TestOpenExistingRefusesMissingDatabase(t *testing.T) {
	path := tempDBPath(t)
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("OpenExisting accepted a missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("OpenExisting created the database")
	}
}

func TestOpenExistingRefusesOtherMigrationSets(t *testing.T) {
	t.Run("older", func(t *testing.T) {
		path := tempDBPath(t)
		createCurrentDB(t, path)
		db := rawDB(t, path)
		missing := lastMigration(t)
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = ?`, missing); err != nil {
			t.Fatal(err)
		}
		db.Close()

		_, err := OpenExisting(path)
		if err == nil || !strings.Contains(err.Error(), "installed binary") {
			t.Fatalf("OpenExisting = %v, want a refusal naming the installed binary", err)
		}
		db = rawDB(t, path)
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, missing).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("OpenExisting applied %s: an existing database must never be migrated", missing)
		}
	})

	t.Run("newer", func(t *testing.T) {
		path := tempDBPath(t)
		createCurrentDB(t, path)
		db := rawDB(t, path)
		if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('migrations/9999_future.sql', datetime('now'))`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		_, err := OpenExisting(path)
		if err == nil || !strings.Contains(err.Error(), "installed binary") {
			t.Fatalf("OpenExisting = %v, want a refusal naming the installed binary", err)
		}
	})
}

func TestOpenExistingCreatesAccountsOnACurrentDatabase(t *testing.T) {
	path := tempDBPath(t)
	createCurrentDB(t, path)
	s, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	defer s.Close()

	u, err := s.CreateUser(context.Background(), "alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Role != core.RoleSystemAdmin {
		t.Errorf("role = %q, want %q", u.Role, core.RoleSystemAdmin)
	}
	got, err := s.GetUserByName(context.Background(), "alice")
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetUserByName = %+v, %v; want id %d", got, err, u.ID)
	}
}

func TestOpenExistingWritesWhileTheServerHoldsTheDatabaseOpen(t *testing.T) {
	path := tempDBPath(t)
	server, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	cmd, err := OpenExisting(path)
	if err != nil {
		t.Fatalf("OpenExisting while the server holds the database: %v", err)
	}
	defer cmd.Close()

	if _, err := cmd.CreateUser(context.Background(), "alice", "hash"); err != nil {
		t.Fatalf("CreateUser while the server holds the database: %v", err)
	}
	if _, err := server.GetUserByName(context.Background(), "alice"); err != nil {
		t.Errorf("the server cannot read the account the command made: %v", err)
	}
}
