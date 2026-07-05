package store

import (
	"context"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

// AddComposeDomain publishes a stack service on a host and returns the row
// with its ID. A duplicate domain on the same app fails the UNIQUE(app_id,
// domain) constraint.
func (s *Store) AddComposeDomain(ctx context.Context, d core.ComposeDomain) (core.ComposeDomain, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO compose_domains (app_id, domain, service, port, created_at) VALUES (?, ?, ?, ?, ?)`,
		d.AppID, d.Domain, d.Service, d.Port, fmtTime(time.Now().UTC()))
	if err != nil {
		return core.ComposeDomain{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.ComposeDomain{}, err
	}
	d.ID = id
	return d, nil
}

// ListComposeDomains returns an app's published domains, ordered by domain.
func (s *Store) ListComposeDomains(ctx context.Context, appID int64) ([]core.ComposeDomain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, domain, service, port FROM compose_domains WHERE app_id = ? ORDER BY domain`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var domains []core.ComposeDomain
	for rows.Next() {
		var d core.ComposeDomain
		if err := rows.Scan(&d.ID, &d.AppID, &d.Domain, &d.Service, &d.Port); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// DeleteComposeDomain removes one domain. The appID scope means a request for
// one app can never delete another app's domain.
func (s *Store) DeleteComposeDomain(ctx context.Context, appID, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM compose_domains WHERE id = ? AND app_id = ?`, id, appID)
	return err
}
