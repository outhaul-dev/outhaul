package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

// primaryDomainPort is the port single-container (nixpacks/dockerfile) apps
// listen on — deploy.AppPort. Duplicated here to avoid a store→deploy import.
const primaryDomainPort = 8080

const domainCols = `id, app_id, host, service, port, path, internal_path, tls, created_at`

// AddDomain inserts a route and returns it with its ID, then re-syncs the app's
// primary-domain mirror. A duplicate (app_id, host, path) violates UNIQUE.
func (s *Store) AddDomain(ctx context.Context, d core.Domain) (core.Domain, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Domain{}, err
	}
	defer tx.Rollback()
	d, err = addDomainTx(ctx, tx, d)
	if err != nil {
		return core.Domain{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Domain{}, err
	}
	return d, nil
}

// addDomainTx inserts one route inside an open transaction, sets its ID and
// CreatedAt, and re-syncs the app's primary-domain mirror. Callers own the
// commit, so CreateApp can seed a row in the same transaction as the app insert.
func addDomainTx(ctx context.Context, tx *sql.Tx, d core.Domain) (core.Domain, error) {
	d.CreatedAt = time.Now().UTC()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.AppID, d.Host, d.Service, d.Port, d.Path, d.InternalPath, boolToInt(d.TLS), fmtTime(d.CreatedAt))
	if err != nil {
		return core.Domain{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Domain{}, err
	}
	d.ID = id
	if err := syncPrimaryDomainTx(ctx, tx, d.AppID); err != nil {
		return core.Domain{}, err
	}
	return d, nil
}

// UpdateDomain rewrites one route (scoped to app_id) and re-syncs the primary.
func (s *Store) UpdateDomain(ctx context.Context, d core.Domain) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE domains SET host = ?, service = ?, port = ?, path = ?, internal_path = ?, tls = ?
		 WHERE id = ? AND app_id = ?`,
		d.Host, d.Service, d.Port, d.Path, d.InternalPath, boolToInt(d.TLS), d.ID, d.AppID); err != nil {
		return err
	}
	if err := syncPrimaryDomainTx(ctx, tx, d.AppID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetDomain fetches one route scoped to its app.
func (s *Store) GetDomain(ctx context.Context, appID, id int64) (core.Domain, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains WHERE id = ? AND app_id = ?`, id, appID)
	return scanDomain(row)
}

// ListDomains returns an app's routes, ordered by host then path.
func (s *Store) ListDomains(ctx context.Context, appID int64) ([]core.Domain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+domainCols+` FROM domains WHERE app_id = ? ORDER BY host, path`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListAllDomains returns every route across all apps, tagged with its app's
// name and kind, for the global Domains tab.
func (s *Store) ListAllDomains(ctx context.Context) ([]core.DomainListing, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.app_id, d.host, d.service, d.port, d.path, d.internal_path, d.tls, d.created_at, a.name, a.kind, a.ephemeral, a.pr_number, a.parent_id
		   FROM domains d JOIN apps a ON a.id = d.app_id
		  ORDER BY a.name, d.host, d.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.DomainListing
	for rows.Next() {
		var (
			l         core.DomainListing
			tls       int
			ephemeral int
			createdAt string
		)
		if err := rows.Scan(&l.ID, &l.AppID, &l.Host, &l.Service, &l.Port, &l.Path, &l.InternalPath,
			&tls, &createdAt, &l.AppName, &l.AppKind, &ephemeral, &l.PRNumber, &l.ParentID); err != nil {
			return nil, err
		}
		t, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		l.CreatedAt = t
		l.TLS = tls != 0
		l.Ephemeral = ephemeral != 0
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteDomain removes one route (scoped to app_id) and re-syncs the primary.
func (s *Store) DeleteDomain(ctx context.Context, appID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM domains WHERE id = ? AND app_id = ?`, id, appID); err != nil {
		return err
	}
	if err := syncPrimaryDomainTx(ctx, tx, appID); err != nil {
		return err
	}
	return tx.Commit()
}

// syncPrimaryDomainTx keeps apps.domain in step with the app's domain rows: it
// becomes the first row's host (host, path order), or empty when the app has
// none. This denormalised mirror is what the app-list views read.
func syncPrimaryDomainTx(ctx context.Context, tx *sql.Tx, appID int64) error {
	var host string
	err := tx.QueryRowContext(ctx,
		`SELECT host FROM domains WHERE app_id = ? ORDER BY host, path LIMIT 1`, appID).Scan(&host)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE apps SET domain = ? WHERE id = ?`, host, appID)
	return err
}

func scanDomain(row scanner) (core.Domain, error) {
	var (
		d         core.Domain
		tls       int
		createdAt string
	)
	if err := row.Scan(&d.ID, &d.AppID, &d.Host, &d.Service, &d.Port, &d.Path, &d.InternalPath, &tls, &createdAt); err != nil {
		return core.Domain{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Domain{}, err
	}
	d.CreatedAt = t
	d.TLS = tls != 0
	return d, nil
}
