package store

import (
	"context"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
)

// CreateApp inserts an app and returns it with ID and CreatedAt populated.
func (s *Store) CreateApp(ctx context.Context, app core.App) (core.App, error) {
	app.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO apps (name, repo_url, domain, created_at) VALUES (?, ?, ?, ?)`,
		app.Name, app.RepoURL, app.Domain, fmtTime(app.CreatedAt))
	if err != nil {
		return core.App{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.App{}, err
	}
	app.ID = id
	return app, nil
}

func (s *Store) GetApp(ctx context.Context, id int64) (core.App, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, repo_url, domain, created_at FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

func (s *Store) GetAppByName(ctx context.Context, name string) (core.App, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, repo_url, domain, created_at FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

func (s *Store) ListApps(ctx context.Context) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, repo_url, domain, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []core.App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// DeleteApp removes an app and its deployments and env vars.
func (s *Store) DeleteApp(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM deployments WHERE app_id = ?`, id); err != nil {
		return err
	}
	// app_env cascades on app delete, but delete explicitly too so the behavior
	// is robust to a future migration dropping the cascade.
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_env WHERE app_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanApp(row scanner) (core.App, error) {
	var (
		app       core.App
		createdAt string
	)
	if err := row.Scan(&app.ID, &app.Name, &app.RepoURL, &app.Domain, &createdAt); err != nil {
		return core.App{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.App{}, err
	}
	app.CreatedAt = t
	return app, nil
}
