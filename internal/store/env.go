package store

import (
	"context"
	"fmt"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

// SetEnv upserts an env var for an app, encrypting the value at rest.
func (s *Store) SetEnv(ctx context.Context, appID int64, key, value string, isSecret bool) error {
	return s.SetEnvScoped(ctx, appID, key, value, isSecret, core.ScopeShared)
}

// SetEnvScoped upserts an env var with an explicit scope.
func (s *Store) SetEnvScoped(ctx context.Context, appID int64, key, value string, isSecret bool, scope string) error {
	if s.box == nil {
		return fmt.Errorf("store: no secret box configured; cannot store env")
	}
	if scope == "" {
		scope = core.ScopeShared
	}
	enc, err := s.box.Seal([]byte(value))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO app_env (app_id, key, value, is_secret, scope, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(app_id, key) DO UPDATE SET
		   value=excluded.value, is_secret=excluded.is_secret, scope=excluded.scope`,
		appID, key, enc, boolToInt(isSecret), scope, fmtTime(time.Now().UTC()))
	return err
}

// DeleteEnv removes an env var by key.
func (s *Store) DeleteEnv(ctx context.Context, appID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_env WHERE app_id = ? AND key = ?`, appID, key)
	return err
}

// ListEnv returns an app's env vars (decrypted), ordered by key.
func (s *Store) ListEnv(ctx context.Context, appID int64) ([]core.EnvVar, error) {
	if s.box == nil {
		return nil, fmt.Errorf("store: no secret box configured; cannot read env")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, is_secret, scope FROM app_env WHERE app_id = ? ORDER BY key`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vars []core.EnvVar
	for rows.Next() {
		var (
			key, enc, scope string
			isSecret        int
		)
		if err := rows.Scan(&key, &enc, &isSecret, &scope); err != nil {
			return nil, err
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypt env %q: %w", key, err)
		}
		vars = append(vars, core.EnvVar{Key: key, Value: string(plain), IsSecret: isSecret != 0, Scope: scope})
	}
	return vars, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
