package store

import (
	"context"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// AttachDatabase links app to database, injecting the connection URL as envVar
// at deploy time. It rejects an invalid env var, a database in a different
// project than the app, and a duplicate env var on the same app.
func (s *Store) AttachDatabase(ctx context.Context, appID, databaseID int64, envVar string) (core.Attachment, error) {
	if !core.ValidEnvVar(envVar) {
		return core.Attachment{}, fmt.Errorf("env var must be UPPER_SNAKE_CASE, got %q", envVar)
	}
	app, err := s.GetApp(ctx, appID)
	if err != nil {
		return core.Attachment{}, err
	}
	db, err := s.GetDatabase(ctx, databaseID)
	if err != nil {
		return core.Attachment{}, err
	}
	if app.ProjectID != db.ProjectID {
		return core.Attachment{}, fmt.Errorf("database %q is in a different project than app %q", db.Name, app.Name)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments (app_id, database_id, env_var, created_at) VALUES (?, ?, ?, ?)`,
		appID, databaseID, envVar, fmtTime(time.Now().UTC()))
	if err != nil {
		return core.Attachment{}, fmt.Errorf("attach database (env var %q may already be used): %w", envVar, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Attachment{}, err
	}
	return core.Attachment{ID: id, AppID: appID, DatabaseID: databaseID, EnvVar: envVar}, nil
}

// DetachDatabase removes one attachment by id, scoped to the app.
func (s *Store) DetachDatabase(ctx context.Context, appID, attachmentID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM attachments WHERE id = ? AND app_id = ?`, attachmentID, appID)
	return err
}

// ListAttachments returns an app's attachments, ordered by env var.
func (s *Store) ListAttachments(ctx context.Context, appID int64) ([]core.Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, database_id, env_var FROM attachments WHERE app_id = ? ORDER BY env_var`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Attachment
	for rows.Next() {
		var a core.Attachment
		if err := rows.Scan(&a.ID, &a.AppID, &a.DatabaseID, &a.EnvVar); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AttachmentsForDatabase returns every attachment referencing databaseID, for
// the delete-database guard and preview propagation.
func (s *Store) AttachmentsForDatabase(ctx context.Context, databaseID int64) ([]core.Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, database_id, env_var FROM attachments WHERE database_id = ? ORDER BY app_id`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Attachment
	for rows.Next() {
		var a core.Attachment
		if err := rows.Scan(&a.ID, &a.AppID, &a.DatabaseID, &a.EnvVar); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
