package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// Snapshot writes a consistent, compacted copy of the database to path, which
// must not exist, and removes every sign-in session from it. The file is mode
// 0600 and has no -wal or -shm beside it.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: snapshot target %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)")
	if err != nil {
		return fmt.Errorf("snapshot: open copy: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := stripSessions(ctx, db); err != nil {
		db.Close()
		return err
	}
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		db.Close()
		return fmt.Errorf("snapshot: integrity_check: %w", err)
	}
	if integrity != "ok" {
		db.Close()
		return fmt.Errorf("snapshot: integrity_check answered %q", integrity)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("snapshot: close copy: %w", err)
	}

	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("snapshot: remove %s: %w", side, err)
		}
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + side); err == nil {
			return fmt.Errorf("snapshot: %s still exists beside the copy", side)
		}
	}
	return nil
}

// StripSessions removes sessions, gate_sessions, route_sessions, gate_codes
// and push_tokens, then VACUUMs so their bytes leave the file.
func (s *Store) StripSessions(ctx context.Context) error {
	return stripSessions(ctx, s.db)
}

func stripSessions(ctx context.Context, db *sql.DB) error {
	for _, table := range []string{"gate_codes", "route_sessions", "gate_sessions", "sessions", "push_tokens"} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("strip %s: %w", table, err)
		}
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("strip sessions: vacuum: %w", err)
	}
	return nil
}
