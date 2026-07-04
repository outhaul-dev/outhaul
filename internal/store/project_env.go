package store

import (
	"context"
	"fmt"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
)

// SetProjectEnv upserts a shared env var for a project, encrypting the value
// at rest. Shared vars reach apps only via ${{project.KEY}} references in
// their own env values (see core.ResolveEnv).
func (s *Store) SetProjectEnv(ctx context.Context, projectID int64, key, value string, isSecret bool) error {
	if s.box == nil {
		return fmt.Errorf("store: no secret box configured; cannot store env")
	}
	enc, err := s.box.Seal([]byte(value))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO project_env (project_id, key, value, is_secret, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, key) DO UPDATE SET value = excluded.value, is_secret = excluded.is_secret`,
		projectID, key, enc, boolToInt(isSecret), fmtTime(time.Now().UTC()))
	return err
}

// DeleteProjectEnv removes a shared env var by key.
func (s *Store) DeleteProjectEnv(ctx context.Context, projectID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_env WHERE project_id = ? AND key = ?`, projectID, key)
	return err
}

// ListProjectEnv returns a project's shared env vars (decrypted), ordered by key.
func (s *Store) ListProjectEnv(ctx context.Context, projectID int64) ([]core.EnvVar, error) {
	if s.box == nil {
		return nil, fmt.Errorf("store: no secret box configured; cannot read env")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, is_secret FROM project_env WHERE project_id = ? ORDER BY key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vars []core.EnvVar
	for rows.Next() {
		var (
			key, enc string
			isSecret int
		)
		if err := rows.Scan(&key, &enc, &isSecret); err != nil {
			return nil, err
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypt project env %q: %w", key, err)
		}
		vars = append(vars, core.EnvVar{Key: key, Value: string(plain), IsSecret: isSecret != 0})
	}
	return vars, rows.Err()
}
