package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

const deploymentCols = `id, app_id, status, reason, image, rollback_of, image_pruned, created_at, started_at, finished_at, tag, from_tag_event`

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
		`INSERT INTO deployments (app_id, status, image, rollback_of, created_at, tag)
		 VALUES (?, ?, ?, ?, ?, COALESCE((SELECT tag FROM deployments WHERE id = ?), ''))`,
		appID, core.StatusQueued, image, rollbackOf, fmtTime(now), rollbackOf)
	if err != nil {
		return core.Deployment{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Deployment{}, err
	}
	var tag string
	if err := s.db.QueryRowContext(ctx, `SELECT tag FROM deployments WHERE id = ?`, id).Scan(&tag); err != nil {
		return core.Deployment{}, err
	}
	return core.Deployment{ID: id, AppID: appID, Status: core.StatusQueued,
		Image: image, RollbackOf: rollbackOf, CreatedAt: now, Tag: tag}, nil
}

// EnqueueTagDeployment reads the app's in-play tags, asks choose whether and
// which tag to deploy, and inserts a queued tag deployment, all in one
// transaction. In play means the non-empty tags of the newest running row plus
// every queued, building or deploying row.
//
// The store has a single connection, so the transaction serializes concurrent
// callers, and choose must not call the store: it would deadlock. It returns
// (dep, true, nil) when it inserts and (core.Deployment{}, false, nil) when
// choose declines.
func (s *Store) EnqueueTagDeployment(ctx context.Context, appID int64, choose func(inPlay []string) (tag string, ok bool)) (core.Deployment, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Deployment{}, false, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT tag FROM deployments
		  WHERE app_id = ? AND tag != '' AND (
		        status IN (?, ?, ?)
		     OR id = (SELECT id FROM deployments WHERE app_id = ? AND status = ?
		              ORDER BY created_at DESC, id DESC LIMIT 1))`,
		appID, core.StatusQueued, core.StatusBuilding, core.StatusDeploying,
		appID, core.StatusRunning)
	if err != nil {
		return core.Deployment{}, false, err
	}
	var inPlay []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			rows.Close()
			return core.Deployment{}, false, err
		}
		inPlay = append(inPlay, tag)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return core.Deployment{}, false, err
	}
	rows.Close()

	tag, ok := choose(inPlay)
	if !ok {
		return core.Deployment{}, false, nil
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO deployments (app_id, status, created_at, tag, from_tag_event) VALUES (?, ?, ?, ?, ?)`,
		appID, core.StatusQueued, fmtTime(now), tag, boolToInt(true))
	if err != nil {
		return core.Deployment{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Deployment{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.Deployment{}, false, err
	}
	return core.Deployment{ID: id, AppID: appID, Status: core.StatusQueued,
		CreatedAt: now, Tag: tag, FromTagEvent: true}, true, nil
}

// LiveDeployment returns the app's newest running deployment, or nil.
func (s *Store) LiveDeployment(ctx context.Context, appID int64) (*core.Deployment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app_id = ? AND status = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, appID, core.StatusRunning)
	d, err := scanDeployment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SetDeploymentTag records the tag a deployment is building.
func (s *Store) SetDeploymentTag(ctx context.Context, id int64, tag string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET tag = ? WHERE id = ?`, tag, id)
	return err
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

// LastDeploymentAt returns the newest deployment's creation time for an app,
// ok=false when the app has no deployments.
func (s *Store) LastDeploymentAt(ctx context.Context, appID int64) (time.Time, bool, error) {
	var maxCreated sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM deployments WHERE app_id = ?`, appID).Scan(&maxCreated); err != nil {
		return time.Time{}, false, err
	}
	if !maxCreated.Valid {
		return time.Time{}, false, nil
	}
	t, err := parseTime(maxCreated.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// CountDeployments returns the total number of deployment rows.
func (s *Store) CountDeployments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments`).Scan(&n)
	return n, err
}

// CountActiveDeployments returns how many deployments are building or
// deploying right now.
func (s *Store) CountActiveDeployments(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deployments WHERE status IN (?, ?)`,
		core.StatusBuilding, core.StatusDeploying).Scan(&n)
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

// SupersedeOthers retires every other running deployment of an app once keepID
// has taken traffic: the blue-green cutover removes the old containers but not
// their rows, so without this they linger as "running" forever. Only rows in
// the running state are touched (running -> superseded is the sole legal edge);
// finished_at is left as first recorded. Returns the number retired.
func (s *Store) SupersedeOthers(ctx context.Context, appID, keepID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?
		 WHERE app_id = ? AND id <> ? AND status = ?`,
		core.StatusSuperseded, appID, keepID, core.StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetImage records the built image tag for a deployment.
func (s *Store) SetImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET image = ? WHERE id = ?`, image, id)
	return err
}

// MarkImagePruned flags every deployment whose image is the given tag as
// pruned. Rollback rows share their source's tag, so the flag must land on
// all of them — a Rollback button over a removed image would lie.
func (s *Store) MarkImagePruned(ctx context.Context, image string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET image_pruned = 1 WHERE image = ?`, image)
	return err
}

// RetainedImages returns the distinct image tags of all unpruned
// image-bearing deployments — everything the pruner's orphan reconciliation
// must keep on the host.
func (s *Store) RetainedImages(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT image FROM deployments WHERE image != '' AND image_pruned = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []string
	for rows.Next() {
		var img string
		if err := rows.Scan(&img); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
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
		fromTag    int
	)
	if err := row.Scan(&d.ID, &d.AppID, &d.Status, &d.Reason, &d.Image,
		&d.RollbackOf, &d.ImagePruned, &createdAt, &startedAt, &finishedAt,
		&d.Tag, &fromTag); err != nil {
		return core.Deployment{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Deployment{}, err
	}
	d.CreatedAt = t
	d.FromTagEvent = fromTag != 0
	if d.StartedAt, err = scanTime(startedAt); err != nil {
		return core.Deployment{}, err
	}
	if d.FinishedAt, err = scanTime(finishedAt); err != nil {
		return core.Deployment{}, err
	}
	return d, nil
}
