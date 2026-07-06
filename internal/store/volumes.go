package store

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

const volumeCols = `id, app_id, name, mount_path, created_at`

var volumeSlugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// deriveVolumeName builds the immutable Docker volume name for an app volume:
// outhaul-<app>-<slug(path)>. App names are unique and valid Docker
// identifiers and the mount path is unique per app, so the result is globally
// unique and a valid Docker volume name.
func deriveVolumeName(appName, mountPath string) string {
	slug := strings.Trim(volumeSlugStrip.ReplaceAllString(strings.ToLower(mountPath), "-"), "-")
	return "outhaul-" + appName + "-" + slug
}

// AddVolume derives the volume's Docker name from the app name and mount path,
// inserts the row, and returns it. A duplicate (app_id, mount_path) or name
// violates a UNIQUE constraint.
func (s *Store) AddVolume(ctx context.Context, appID int64, mountPath string) (core.Volume, error) {
	var appName string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM apps WHERE id = ?`, appID).Scan(&appName); err != nil {
		return core.Volume{}, err
	}
	v := core.Volume{
		AppID:     appID,
		Name:      deriveVolumeName(appName, mountPath),
		MountPath: mountPath,
		CreatedAt: time.Now().UTC(),
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO volumes (app_id, name, mount_path, created_at) VALUES (?, ?, ?, ?)`,
		v.AppID, v.Name, v.MountPath, fmtTime(v.CreatedAt))
	if err != nil {
		return core.Volume{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Volume{}, err
	}
	v.ID = id
	return v, nil
}

// UpdateVolume changes only the mount path (the Docker name is immutable).
func (s *Store) UpdateVolume(ctx context.Context, appID, id int64, mountPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE volumes SET mount_path = ? WHERE id = ? AND app_id = ?`, mountPath, id, appID)
	return err
}

// GetVolume fetches one volume scoped to its app.
func (s *Store) GetVolume(ctx context.Context, appID, id int64) (core.Volume, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+volumeCols+` FROM volumes WHERE id = ? AND app_id = ?`, id, appID)
	return scanVolume(row)
}

// ListVolumes returns an app's volumes, ordered by mount path.
func (s *Store) ListVolumes(ctx context.Context, appID int64) ([]core.Volume, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+volumeCols+` FROM volumes WHERE app_id = ? ORDER BY mount_path`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Volume
	for rows.Next() {
		v, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListAllVolumes returns every app volume tagged with its app's name and kind,
// for the global Volumes tab.
func (s *Store) ListAllVolumes(ctx context.Context) ([]core.VolumeListing, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT v.id, v.app_id, v.name, v.mount_path, v.created_at, a.name, a.kind
		   FROM volumes v JOIN apps a ON a.id = v.app_id
		  ORDER BY a.name, v.mount_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.VolumeListing
	for rows.Next() {
		var (
			l         core.VolumeListing
			createdAt string
		)
		if err := rows.Scan(&l.ID, &l.AppID, &l.Name, &l.MountPath, &createdAt, &l.AppName, &l.AppKind); err != nil {
			return nil, err
		}
		t, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		l.CreatedAt = t
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteVolume removes the row only. The underlying Docker volume is left in
// place (it becomes a reclaimable orphan in the inventory).
func (s *Store) DeleteVolume(ctx context.Context, appID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM volumes WHERE id = ? AND app_id = ?`, id, appID)
	return err
}

func scanVolume(row scanner) (core.Volume, error) {
	var (
		v         core.Volume
		createdAt string
	)
	if err := row.Scan(&v.ID, &v.AppID, &v.Name, &v.MountPath, &createdAt); err != nil {
		return core.Volume{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Volume{}, err
	}
	v.CreatedAt = t
	return v, nil
}
