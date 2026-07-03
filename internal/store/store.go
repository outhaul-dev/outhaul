// Package store is Slipway's SQLite persistence layer and job queue. It depends
// only on internal/core. Writes are serialized (single open connection) so the
// pure-Go driver never trips on concurrent writers; WAL + busy_timeout keep
// reads responsive.
package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, applies pragmas,
// and runs embedded migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serializes all writes, which is what we want with the
	// pure-Go driver. Deploys are I/O-bound (clone/build), not DB-bound, so this
	// is not a throughput concern for M1.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
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
