package store

import (
	"context"
	"fmt"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

const databaseCols = `id, project_id, name, engine, image, username, password, db_name, ext_port, status, reason, created_at`

// CreateDatabase inserts a managed database row in status creating, encrypting
// the password at rest. The returned Database keeps the plaintext password so
// the caller can provision without a re-read.
func (s *Store) CreateDatabase(ctx context.Context, d core.Database) (core.Database, error) {
	if s.box == nil {
		return core.Database{}, fmt.Errorf("store: no secret box configured; cannot store database credentials")
	}
	enc, err := s.box.Seal([]byte(d.Password))
	if err != nil {
		return core.Database{}, err
	}
	if d.ProjectID == 0 {
		d.ProjectID = core.DefaultProjectID
	}
	d.Status = core.DBCreating
	d.Reason = ""
	d.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO databases (project_id, name, engine, image, username, password, db_name, ext_port, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ProjectID, d.Name, d.Engine, d.Image, d.Username, enc, d.DBName, d.ExtPort, d.Status, fmtTime(d.CreatedAt))
	if err != nil {
		return core.Database{}, err
	}
	d.ID, err = res.LastInsertId()
	if err != nil {
		return core.Database{}, err
	}
	return d, nil
}

func (s *Store) GetDatabase(ctx context.Context, id int64) (core.Database, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+databaseCols+` FROM databases WHERE id = ?`, id)
	return s.scanDatabase(row)
}

// ListDatabasesByProject returns a project's databases (passwords decrypted),
// ordered by name.
func (s *Store) ListDatabasesByProject(ctx context.Context, projectID int64) ([]core.Database, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+databaseCols+` FROM databases WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dbs []core.Database
	for rows.Next() {
		d, err := s.scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, d)
	}
	return dbs, rows.Err()
}

// SetDatabaseStatus records a lifecycle state (and failure reason) on the row.
func (s *Store) SetDatabaseStatus(ctx context.Context, id int64, status, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE databases SET status = ?, reason = ? WHERE id = ?`, status, reason, id)
	return err
}

// SetDatabaseExtPort records a new published host port; the caller reprovisions
// the container to apply it.
func (s *Store) SetDatabaseExtPort(ctx context.Context, id int64, port int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE databases SET ext_port = ? WHERE id = ?`, port, id)
	return err
}

// DeleteDatabase removes the row and its backup schedules. The caller is
// responsible for the container and data directory (see dbaas.Manager.Remove).
func (s *Store) DeleteDatabase(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteBackupsForTargetTx(ctx, tx, core.BackupTargetDatabase, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM databases WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverCreatingDatabases marks databases stuck in creating as failed — the
// binary restarted mid-provision and the goroutine doing the work is gone.
// Same idea as RecoverActive for deployments.
func (s *Store) RecoverCreatingDatabases(ctx context.Context, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE databases SET status = ?, reason = ? WHERE status = ?`,
		core.DBFailed, reason, core.DBCreating)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) scanDatabase(row scanner) (core.Database, error) {
	if s.box == nil {
		return core.Database{}, fmt.Errorf("store: no secret box configured; cannot read database credentials")
	}
	var (
		d         core.Database
		enc       string
		createdAt string
	)
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Image,
		&d.Username, &enc, &d.DBName, &d.ExtPort, &d.Status, &d.Reason, &createdAt); err != nil {
		return core.Database{}, err
	}
	plain, err := s.box.Open(enc)
	if err != nil {
		return core.Database{}, fmt.Errorf("decrypt database %q password: %w", d.Name, err)
	}
	d.Password = string(plain)
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Database{}, err
	}
	d.CreatedAt = t
	return d, nil
}
