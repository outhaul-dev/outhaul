package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

// appCols is the column list read into core.App by scanApp (excludes the
// write-only ssh_private_key, which is fetched separately and decrypted).
const appCols = `id, project_id, name, repo_url, domain, created_at, branch, auto_deploy, source, webhook_secret, ssh_public_key, github_repo, kind, compose_path, dockerfile_path, watch_paths, template_id, compose_raw`

// CreateApp inserts an app and returns it with ID and CreatedAt populated. The
// SSH private key (if any) is encrypted at rest.
func (s *Store) CreateApp(ctx context.Context, app core.App) (core.App, error) {
	app.CreatedAt = time.Now().UTC()
	if app.Source == "" {
		app.Source = core.SourcePublic
	}
	if app.Branch == "" {
		app.Branch = "main"
	}
	if app.ProjectID == 0 {
		app.ProjectID = core.DefaultProjectID
	}
	if app.Kind == "" {
		app.Kind = core.KindNixpacks
	}
	encKey, err := s.sealMaybe(app.SSHPrivateKey)
	if err != nil {
		return core.App{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO apps
		   (project_id, name, repo_url, domain, created_at, branch, auto_deploy, source, webhook_secret, ssh_private_key, ssh_public_key, github_repo,
		    kind, compose_path, dockerfile_path, watch_paths, template_id, compose_raw)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ProjectID, app.Name, app.RepoURL, app.Domain, fmtTime(app.CreatedAt),
		app.Branch, boolToInt(app.AutoDeploy), app.Source, app.WebhookSecret, encKey, app.SSHPublicKey, app.GithubRepo,
		app.Kind, app.ComposePath, app.DockerfilePath, joinWatchPaths(app.WatchPaths), app.TemplateID, app.ComposeRaw)
	if err != nil {
		return core.App{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.App{}, err
	}
	app.ID = id
	app.SSHPrivateKey = "" // never keep the plaintext around
	return app, nil
}

// sealMaybe encrypts a non-empty value; empty stays empty.
func (s *Store) sealMaybe(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if s.box == nil {
		return "", fmt.Errorf("store: no secret box configured; cannot store secret")
	}
	return s.box.Seal([]byte(v))
}

func (s *Store) GetApp(ctx context.Context, id int64) (core.App, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

func (s *Store) GetAppByName(ctx context.Context, name string) (core.App, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

func (s *Store) ListApps(ctx context.Context) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
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

// ListAppsByProject returns the apps in one project, ordered by name.
func (s *Store) ListAppsByProject(ctx context.Context, projectID int64) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+appCols+` FROM apps WHERE project_id = ? ORDER BY name`, projectID)
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

// AppByWebhookSecret finds the app whose generic-webhook token matches secret.
func (s *Store) AppByWebhookSecret(ctx context.Context, secret string) (core.App, error) {
	if secret == "" {
		return core.App{}, sql.ErrNoRows
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE webhook_secret = ?`, secret)
	return scanApp(row)
}

// AppsByGithubRepo returns all apps sourced from the given "owner/name".
func (s *Store) AppsByGithubRepo(ctx context.Context, fullName string) ([]core.App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+appCols+` FROM apps WHERE source = ? AND github_repo = ?`, core.SourceGithub, fullName)
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

// SSHPrivateKey returns the decrypted deploy private key for an app.
func (s *Store) SSHPrivateKey(ctx context.Context, appID int64) (string, error) {
	var enc string
	if err := s.db.QueryRowContext(ctx, `SELECT ssh_private_key FROM apps WHERE id = ?`, appID).Scan(&enc); err != nil {
		return "", err
	}
	if enc == "" {
		return "", nil
	}
	if s.box == nil {
		return "", fmt.Errorf("store: no secret box configured; cannot read ssh key")
	}
	plain, err := s.box.Open(enc)
	if err != nil {
		return "", fmt.Errorf("decrypt ssh key: %w", err)
	}
	return string(plain), nil
}

// UpdateAppSettings updates the mutable per-app deploy settings.
func (s *Store) UpdateAppSettings(ctx context.Context, id int64, branch string, autoDeploy bool, watchPaths []string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE apps SET branch = ?, auto_deploy = ?, watch_paths = ? WHERE id = ?`,
		branch, boolToInt(autoDeploy), joinWatchPaths(watchPaths), id)
	return err
}

// UpdateAppComposePath updates where a compose app's compose file lives.
// Domain exposure is managed separately via the compose_domains table.
func (s *Store) UpdateAppComposePath(ctx context.Context, id int64, composePath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE apps SET compose_path = ? WHERE id = ?`, composePath, id)
	return err
}

// UpdateAppDockerfilePath updates where a dockerfile app's Dockerfile lives.
func (s *Store) UpdateAppDockerfilePath(ctx context.Context, id int64, dockerfilePath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE apps SET dockerfile_path = ? WHERE id = ?`, dockerfilePath, id)
	return err
}

// DeleteApp removes an app and its deployments, env vars, and backups.
func (s *Store) DeleteApp(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM deployments WHERE app_id = ?`, id); err != nil {
		return err
	}
	// app_env and compose_domains cascade on app delete, but delete explicitly
	// too so the behavior is robust to a future migration dropping the cascade.
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_env WHERE app_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM compose_domains WHERE app_id = ?`, id); err != nil {
		return err
	}
	if err := deleteBackupsForTargetTx(ctx, tx, core.BackupTargetApp, id); err != nil {
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
		app        core.App
		createdAt  string
		autoDeploy int
		watchPaths string
	)
	if err := row.Scan(&app.ID, &app.ProjectID, &app.Name, &app.RepoURL, &app.Domain, &createdAt,
		&app.Branch, &autoDeploy, &app.Source, &app.WebhookSecret, &app.SSHPublicKey, &app.GithubRepo,
		&app.Kind, &app.ComposePath, &app.DockerfilePath, &watchPaths, &app.TemplateID, &app.ComposeRaw); err != nil {
		return core.App{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.App{}, err
	}
	app.CreatedAt = t
	app.AutoDeploy = autoDeploy != 0
	app.WatchPaths = splitWatchPaths(watchPaths)
	return app, nil
}

// Watch paths are stored as one newline-separated TEXT column; blank lines and
// surrounding whitespace never survive a round-trip.
func joinWatchPaths(patterns []string) string {
	return strings.Join(patterns, "\n")
}

func splitWatchPaths(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
