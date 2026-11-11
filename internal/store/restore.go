package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// ErrDatabaseInUse means another process holds the database open.
var ErrDatabaseInUse = errors.New("store: database is in use")

// ErrNotStateCopy means the file is not an Outhaul database.
var ErrNotStateCopy = errors.New("store: not an Outhaul state copy")

// ErrNewerStateCopy means the copy records a migration this binary does not
// embed.
var ErrNewerStateCopy = errors.New("store: the copy comes from a newer Outhaul")

// CheckDatabaseIdle reports whether another process has path open, by taking
// an exclusive lock with no busy timeout. A missing file is idle. It folds a
// -wal left by a crash into the file.
func CheckDatabaseIdle(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+
		"?mode=rw&_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
		if isLocked(err) {
			return ErrDatabaseInUse
		}
		return err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	return nil
}

// HasUserInFile reports whether the database at path has an account. It opens
// the file read-only and never creates one.
func HasUserInFile(path string) (bool, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro")
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return n > 0, nil
}

// CheckStateCopy verifies path is a SQLite database with a schema_migrations
// table whose every row this binary embeds, and returns those migration names.
func CheckStateCopy(path string) ([]string, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`PRAGMA integrity_check`)
	if err != nil {
		if isNotADatabase(err) {
			return nil, fmt.Errorf("%w: %s is not a SQLite database", ErrNotStateCopy, path)
		}
		return nil, fmt.Errorf("integrity_check failed for %s: %w", path, err)
	}
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, fmt.Errorf("integrity_check failed for %s: %w", path, err)
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("integrity_check failed for %s: %w", path, err)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("integrity_check failed for %s: %s", path, strings.Join(problems, "; "))
	}

	rows, err = db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, fmt.Errorf("%w: %s has no schema_migrations", ErrNotStateCopy, path)
		}
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	known := map[string]bool{}
	embedded, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	for _, n := range embedded {
		known[n] = true
	}
	sort.Strings(names)
	for _, n := range names {
		if !known[n] {
			return nil, fmt.Errorf("%w: %s; install that version or newer, then restore", ErrNewerStateCopy, n)
		}
	}
	return names, nil
}

// AppliedMigrations returns the migration names recorded in the database.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// StateCounts returns how many accounts, apps and managed databases the
// database holds, for the restore summary.
func (s *Store) StateCounts(ctx context.Context) (accounts, apps, databases int, err error) {
	for _, q := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM users`, &accounts},
		{`SELECT COUNT(*) FROM apps`, &apps},
		{`SELECT COUNT(*) FROM databases`, &databases},
	} {
		if err := s.db.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return 0, 0, 0, err
		}
	}
	return accounts, apps, databases, nil
}

// restoreMarkerKey holds the first-start marker in settings.
const restoreMarkerKey = "state_restore"

// RestoreMarker returns the pending or finished restore notice. ok is false
// when there is none.
func (s *Store) RestoreMarker(ctx context.Context) (core.RestoreMarker, bool, error) {
	v, ok, err := s.GetSetting(ctx, restoreMarkerKey)
	if err != nil || !ok {
		return core.RestoreMarker{}, false, err
	}
	var m core.RestoreMarker
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return core.RestoreMarker{}, false, fmt.Errorf("store: decode %s: %w", restoreMarkerKey, err)
	}
	return m, true, nil
}

// SetRestoreMarker stores the first-start marker.
func (s *Store) SetRestoreMarker(ctx context.Context, m core.RestoreMarker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, restoreMarkerKey, string(b))
}

// ClearRestoreMarker removes the first-start marker.
func (s *Store) ClearRestoreMarker(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, restoreMarkerKey)
	return err
}

// isLocked recognises the driver's busy/locked answers, which are how an open
// database is told apart from a broken one.
func isLocked(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database is locked") || strings.Contains(s, "sqlite_busy")
}

// isNotADatabase recognises a file SQLite will not read as a database.
func isNotADatabase(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not a database")
}
