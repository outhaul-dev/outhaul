// Package store is Outhaul's SQLite persistence layer and job queue. It depends
// only on internal/core. Writes are serialized (single open connection) so the
// pure-Go driver never trips on concurrent writers; WAL + busy_timeout keep
// reads responsive.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/outhaul-dev/outhaul/internal/secret"
	_ "modernc.org/sqlite"
)

// Store wraps the database handle.
type Store struct {
	db  *sql.DB
	box *secret.Box // encrypts app_env values; may be nil for non-env callers

	reservedHost atomic.Value // string: the host no domain row may use; see SetReservedHost
}

// Open opens (creating if needed) the SQLite database at path, applies pragmas,
// runs embedded migrations, and uses box to encrypt/decrypt env values. box may
// be nil for callers that never touch app_env.
func Open(path string, box *secret.Box) (*Store, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, box: box}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// openSQLite opens path with the pragmas every Outhaul connection uses. With
// create false it opens an existing file read-write and never creates one.
func openSQLite(path string, create bool) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path)
	if create {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	} else {
		dsn += "?mode=rw&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serializes all writes, which is what we want with the
	// pure-Go driver. Deploys are I/O-bound (clone/build), not DB-bound, so this
	// is not a throughput concern for M1.
	db.SetMaxOpenConns(1)
	return db, nil
}

// OpenExisting opens an Outhaul database that already exists, without creating
// or migrating it. It opens no secret box: callers that only create an account
// never touch a sealed value. The file's schema_migrations must hold exactly
// this binary's migrations, or nothing is changed.
func OpenExisting(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no Outhaul database at %s; install Outhaul first", path)
		}
		return nil, err
	}
	db, err := openSQLite(path, false)
	if err != nil {
		return nil, err
	}
	if err := checkMigrationsCurrent(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// checkMigrationsCurrent refuses a database whose migration set is not exactly
// the one this binary embeds: an older file would need a migration this command
// must not run, and a newer file records a migration this binary doesn't know.
func checkMigrationsCurrent(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return errors.New("no schema_migrations: this is not an Outhaul database")
		}
		return err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[name] = true
	}
	for name := range applied {
		if !known[name] {
			return fmt.Errorf("the database records migration %s, which this binary doesn't have; use the installed binary", name)
		}
	}
	for _, name := range names {
		if !applied[name] {
			return fmt.Errorf("the database is missing migration %s; start Outhaul once to migrate it, then use the installed binary", name)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// --- time helpers: timestamps are stored as RFC3339Nano TEXT ---

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// scanTime reads a nullable TEXT timestamp column into *time.Time.
func scanTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
