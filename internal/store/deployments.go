package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

const deploymentCols = `id, app_id, status, reason, image, rollback_of, created_at, started_at, finished_at`

// CreateDeployment inserts a new attempt in the queued state.
func (s *Store) CreateDeployment(ctx context.Context, appID int64) (core.Deployment, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO deployments (app_id, status, created_at) VALUES (?, ?, ?)`,
		appID, core.StatusQueued, fmtTime(now))
	if err != nil {
		return core.Deployment{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Deployment{}, err
	}
	return core.Deployment{ID: id, AppID: appID, Status: core.StatusQueued, CreatedAt: now}, nil
}

// CreateRollback inserts a queued attempt that reuses a previous deployment's
// image. The pre-set image is what makes the pipeline skip the clone and
// build; rollbackOf records provenance for the UI.
func (s *Store) CreateRollback(ctx context.Context, appID int64, image string, rollbackOf int64) (core.Deployment, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO deployments (app_id, status, image, rollback_of, created_at) VALUES (?, ?, ?, ?, ?)`,
		appID, core.StatusQueued, image, rollbackOf, fmtTime(now))
	if err != nil {
		return core.Deployment{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Deployment{}, err
	}
	return core.Deployment{ID: id, AppID: appID, Status: core.StatusQueued,
		Image: image, RollbackOf: rollbackOf, CreatedAt: now}, nil
}

func (s *Store) GetDeployment(ctx context.Context, id int64) (core.Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id)
	return scanDeployment(row)
}

// ListDeploymentsForApp returns an app's deployments, newest first.
func (s *Store) ListDeploymentsForApp(ctx context.Context, appID int64) ([]core.Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app_id = ? ORDER BY created_at DESC, id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ds []core.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// LatestDeploymentForApp returns the most recent attempt, or nil if none.
func (s *Store) LatestDeploymentForApp(ctx context.Context, appID int64) (*core.Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, appID)
	d, err := scanDeployment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListRecentDeployments returns the most recent deployments across all apps,
// newest first, capped at limit.
func (s *Store) ListRecentDeployments(ctx context.Context, limit int) ([]core.Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ds []core.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}

// CountDeployments returns the total number of deployment rows.
func (s *Store) CountDeployments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments`).Scan(&n)
	return n, err
}

// ClaimDeployment atomically moves a deployment from queued -> building and
// stamps StartedAt. Returns false if it was not queued (lost the race).
func (s *Store) ClaimDeployment(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, started_at = ?
		 WHERE id = ? AND status = ?`,
		core.StatusBuilding, fmtTime(time.Now().UTC()), id, core.StatusQueued)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetStatus performs a guarded state transition from -> to. It applies only if
// the transition is legal (core.CanTransition) and the row is still in the
// expected 'from' status. Terminal transitions stamp FinishedAt and store the
// reason. Returns false if nothing was updated.
func (s *Store) SetStatus(ctx context.Context, id int64, from, to core.DeployStatus, reason string) (bool, error) {
	if !core.CanTransition(from, to) {
		return false, nil
	}

	var (
		finished any // NULL unless terminal
	)
	if to.IsTerminal() {
		finished = fmtTime(time.Now().UTC())
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, reason = ?, finished_at = ?
		 WHERE id = ? AND status = ?`,
		to, reason, finished, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetImage records the built image tag for a deployment.
func (s *Store) SetImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET image = ? WHERE id = ?`, image, id)
	return err
}

// NextClaimable returns the oldest queued deployment whose app has no active
// (building/deploying) deployment, or nil if none is claimable. This enforces
// per-app serialization while allowing different apps to run concurrently.
func (s *Store) NextClaimable(ctx context.Context) (*core.Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments d
		 WHERE d.status = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM deployments a
		       WHERE a.app_id = d.app_id AND a.status IN (?, ?)
		   )
		 ORDER BY d.created_at ASC, d.id ASC
		 LIMIT 1`,
		core.StatusQueued, core.StatusBuilding, core.StatusDeploying)

	d, err := scanDeployment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RecoverActive marks every deployment left active (building/deploying) as
// failed with the given reason. Run once on startup so a crash never leaves an
// attempt orphaned in an active state. Queued rows are left untouched. Returns
// the number of rows recovered.
func (s *Store) RecoverActive(ctx context.Context, reason string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, reason = ?, finished_at = ?
		 WHERE status IN (?, ?)`,
		core.StatusFailed, reason, fmtTime(time.Now().UTC()),
		core.StatusBuilding, core.StatusDeploying)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func scanDeployment(row scanner) (core.Deployment, error) {
	var (
		d          core.Deployment
		createdAt  string
		startedAt  sql.NullString
		finishedAt sql.NullString
	)
	if err := row.Scan(&d.ID, &d.AppID, &d.Status, &d.Reason, &d.Image,
		&d.RollbackOf, &createdAt, &startedAt, &finishedAt); err != nil {
		return core.Deployment{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Deployment{}, err
	}
	d.CreatedAt = t
	if d.StartedAt, err = scanTime(startedAt); err != nil {
		return core.Deployment{}, err
	}
	if d.FinishedAt, err = scanTime(finishedAt); err != nil {
		return core.Deployment{}, err
	}
	return d, nil
}
