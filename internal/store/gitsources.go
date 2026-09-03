package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// gitSourceSelect reads a source with its per-kind credentials. The LEFT JOIN
// keeps a source row readable even when this build knows nothing about its
// kind, so an unknown kind degrades to "not installed" rather than an error.
const gitSourceSelect = `SELECT
	s.id, s.kind, s.account_login, s.account_type, s.created_at,
	COALESCE(g.app_id, 0), COALESCE(g.slug, ''), COALESCE(g.private_key, ''),
	COALESCE(g.webhook_secret, ''), COALESCE(g.client_id, ''),
	COALESCE(g.client_secret, ''), COALESCE(g.installation_id, 0)
	FROM git_sources s LEFT JOIN github_app_sources g ON g.source_id = s.id`

// ListGitSources returns every connected source: named ones alphabetically
// first, still-pending ones last.
func (s *Store) ListGitSources(ctx context.Context) ([]core.GitSource, error) {
	rows, err := s.db.QueryContext(ctx, gitSourceSelect+
		` ORDER BY (s.account_login = ''), s.account_login COLLATE NOCASE, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.GitSource
	for rows.Next() {
		src, err := s.scanGitSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// GetGitSource returns one source by id, or ok=false if there is none.
func (s *Store) GetGitSource(ctx context.Context, id int64) (core.GitSource, bool, error) {
	return s.oneGitSource(ctx, gitSourceSelect+` WHERE s.id = ?`, id)
}

// GitSourceByGithubAppID resolves the source that owns a GitHub App id. This is
// the webhook routing lookup: GitHub names the App in every delivery header.
func (s *Store) GitSourceByGithubAppID(ctx context.Context, appID int64) (core.GitSource, bool, error) {
	return s.oneGitSource(ctx, gitSourceSelect+` WHERE g.app_id = ?`, appID)
}

func (s *Store) oneGitSource(ctx context.Context, query string, arg any) (core.GitSource, bool, error) {
	src, err := s.scanGitSource(s.db.QueryRowContext(ctx, query, arg))
	if err == sql.ErrNoRows {
		return core.GitSource{}, false, nil
	}
	if err != nil {
		return core.GitSource{}, false, err
	}
	return src, true, nil
}

func (s *Store) scanGitSource(row scanner) (core.GitSource, error) {
	var (
		src                 core.GitSource
		createdAt           string
		encPK, encWH, encCS string
	)
	if err := row.Scan(&src.ID, &src.Kind, &src.AccountLogin, &src.AccountType, &createdAt,
		&src.GithubApp.AppID, &src.GithubApp.Slug, &encPK, &encWH,
		&src.GithubApp.ClientID, &encCS, &src.GithubApp.InstallationID); err != nil {
		return core.GitSource{}, err
	}
	if t, err := parseTime(createdAt); err == nil {
		src.CreatedAt = t
	}
	if src.Kind != core.GitSourceGithubApp {
		return src, nil
	}
	if s.box == nil {
		return core.GitSource{}, fmt.Errorf("store: no secret box configured; cannot read git source")
	}
	pk, err := s.box.Open(encPK)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt private_key: %w", err)
	}
	wh, err := s.box.Open(encWH)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt webhook_secret: %w", err)
	}
	cs, err := s.box.Open(encCS)
	if err != nil {
		return core.GitSource{}, fmt.Errorf("decrypt client_secret: %w", err)
	}
	src.GithubApp.PrivateKey = string(pk)
	src.GithubApp.WebhookSecret = string(wh)
	src.GithubApp.ClientSecret = string(cs)
	return src, nil
}

// CreateGithubAppSource records a freshly-created GitHub App, before it has been
// installed. Both rows land in one transaction so a source never exists without
// its credentials.
func (s *Store) CreateGithubAppSource(ctx context.Context, creds core.GithubAppCreds) (core.GitSource, error) {
	if s.box == nil {
		return core.GitSource{}, fmt.Errorf("store: no secret box configured; cannot store git source")
	}
	encPK, err := s.box.Seal([]byte(creds.PrivateKey))
	if err != nil {
		return core.GitSource{}, err
	}
	encWH, err := s.box.Seal([]byte(creds.WebhookSecret))
	if err != nil {
		return core.GitSource{}, err
	}
	encCS, err := s.box.Seal([]byte(creds.ClientSecret))
	if err != nil {
		return core.GitSource{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.GitSource{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO git_sources (kind, account_login, account_type, created_at) VALUES (?, '', '', ?)`,
		core.GitSourceGithubApp, fmtTime(now))
	if err != nil {
		return core.GitSource{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.GitSource{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO github_app_sources
		   (source_id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, creds.AppID, creds.Slug, encPK, encWH, creds.ClientID, encCS, creds.InstallationID); err != nil {
		return core.GitSource{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.GitSource{}, err
	}
	return core.GitSource{
		ID: id, Kind: core.GitSourceGithubApp, CreatedAt: now, GithubApp: creds,
	}, nil
}

// BindGithubInstallation completes a source: it records the installation the
// operator chose on GitHub, and the account that installation belongs to.
func (s *Store) BindGithubInstallation(ctx context.Context, sourceID, installationID int64, login, accountType string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE github_app_sources SET installation_id = ? WHERE source_id = ?`,
		installationID, sourceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE git_sources SET account_login = ?, account_type = ? WHERE id = ?`,
		login, accountType, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetGitSourceAccount backfills the account a source belongs to. Sources
// migrated from the pre-0022 single-App record have no account recorded,
// because GitHub was never asked.
func (s *Store) SetGitSourceAccount(ctx context.Context, sourceID int64, login, accountType string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE git_sources SET account_login = ?, account_type = ? WHERE id = ?`,
		login, accountType, sourceID)
	return err
}

// DeleteGitSource removes a source; its credential row cascades. Callers must
// refuse to call this while apps still reference the source.
func (s *Store) DeleteGitSource(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM git_sources WHERE id = ?`, id)
	return err
}
