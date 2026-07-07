package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// GithubApp returns the configured GitHub App, or ok=false if none is set up.
func (s *Store) GithubApp(ctx context.Context) (core.GithubApp, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id, created_at
		   FROM github_app WHERE id = 1`)
	var (
		ga                             core.GithubApp
		encPK, encWH, encCS, createdAt string
	)
	err := row.Scan(&ga.AppID, &ga.Slug, &encPK, &encWH, &ga.ClientID, &encCS, &ga.InstallationID, &createdAt)
	if err == sql.ErrNoRows {
		return core.GithubApp{}, false, nil
	}
	if err != nil {
		return core.GithubApp{}, false, err
	}
	if s.box == nil {
		return core.GithubApp{}, false, fmt.Errorf("store: no secret box configured; cannot read github app")
	}
	pk, err := s.box.Open(encPK)
	if err != nil {
		return core.GithubApp{}, false, fmt.Errorf("decrypt private_key: %w", err)
	}
	wh, err := s.box.Open(encWH)
	if err != nil {
		return core.GithubApp{}, false, fmt.Errorf("decrypt webhook_secret: %w", err)
	}
	cs, err := s.box.Open(encCS)
	if err != nil {
		return core.GithubApp{}, false, fmt.Errorf("decrypt client_secret: %w", err)
	}
	ga.PrivateKey, ga.WebhookSecret, ga.ClientSecret = string(pk), string(wh), string(cs)
	if t, err := parseTime(createdAt); err == nil {
		ga.CreatedAt = t
	}
	return ga, true, nil
}

// SetGithubApp upserts the single App row, encrypting the private key, webhook
// secret, and client secret. It preserves any existing installation id.
func (s *Store) SetGithubApp(ctx context.Context, ga core.GithubApp) error {
	if s.box == nil {
		return fmt.Errorf("store: no secret box configured; cannot store github app")
	}
	encPK, err := s.box.Seal([]byte(ga.PrivateKey))
	if err != nil {
		return err
	}
	encWH, err := s.box.Seal([]byte(ga.WebhookSecret))
	if err != nil {
		return err
	}
	encCS, err := s.box.Seal([]byte(ga.ClientSecret))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO github_app (id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id, created_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, COALESCE((SELECT installation_id FROM github_app WHERE id = 1), 0), ?)
		 ON CONFLICT(id) DO UPDATE SET
		   app_id = excluded.app_id, slug = excluded.slug, private_key = excluded.private_key,
		   webhook_secret = excluded.webhook_secret, client_id = excluded.client_id,
		   client_secret = excluded.client_secret`,
		ga.AppID, ga.Slug, encPK, encWH, ga.ClientID, encCS, fmtTime(time.Now().UTC()))
	return err
}

// SetInstallationID records the installation selected by the operator.
func (s *Store) SetInstallationID(ctx context.Context, installationID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE github_app SET installation_id = ? WHERE id = 1`, installationID)
	return err
}
