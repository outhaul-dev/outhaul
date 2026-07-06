package store

import (
	"context"
	"database/sql"

	"github.com/james-smart/outhaul/internal/core"
)

// GetPreviewConfig returns an app's preview config, or the disabled default
// when no row exists.
func (s *Store) GetPreviewConfig(ctx context.Context, appID int64) (core.PreviewConfig, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT enabled, base_domain, post_pr_comment, allow_fork_prs, idle_ttl_days, max_concurrent
		   FROM preview_configs WHERE app_id = ?`, appID)
	c := core.DefaultPreviewConfig(appID)
	var enabled, comment, fork int
	err := row.Scan(&enabled, &c.BaseDomain, &comment, &fork, &c.IdleTTLDays, &c.MaxConcurrent)
	if err == sql.ErrNoRows {
		return core.DefaultPreviewConfig(appID), nil
	}
	if err != nil {
		return core.PreviewConfig{}, err
	}
	c.Enabled, c.PostPRComment, c.AllowForkPRs = enabled != 0, comment != 0, fork != 0
	return c, nil
}

// SetPreviewConfig upserts an app's preview config.
func (s *Store) SetPreviewConfig(ctx context.Context, c core.PreviewConfig) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preview_configs (app_id, enabled, base_domain, post_pr_comment, allow_fork_prs, idle_ttl_days, max_concurrent)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(app_id) DO UPDATE SET
		   enabled=excluded.enabled, base_domain=excluded.base_domain,
		   post_pr_comment=excluded.post_pr_comment, allow_fork_prs=excluded.allow_fork_prs,
		   idle_ttl_days=excluded.idle_ttl_days, max_concurrent=excluded.max_concurrent`,
		c.AppID, boolToInt(c.Enabled), c.BaseDomain, boolToInt(c.PostPRComment),
		boolToInt(c.AllowForkPRs), c.IdleTTLDays, c.MaxConcurrent)
	return err
}

// GetPreviewByPR returns the ephemeral child app for a parent + PR number.
func (s *Store) GetPreviewByPR(ctx context.Context, parentID int64, pr int) (core.App, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+appCols+` FROM apps WHERE parent_id = ? AND pr_number = ?`, parentID, pr)
	return scanApp(row)
}

// ListPreviewsForParent returns a parent app's active preview children.
func (s *Store) ListPreviewsForParent(ctx context.Context, parentID int64) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps WHERE parent_id = ? ORDER BY pr_number`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListPreviews returns every ephemeral app across all parents.
func (s *Store) ListPreviews(ctx context.Context) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps WHERE ephemeral = 1 ORDER BY parent_id, pr_number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
