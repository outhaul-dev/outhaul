package store

import (
	"context"
	"errors"
	"time"

	"github.com/slipwaydev/slipway/internal/core"
)

// ErrProjectNotEmpty is returned by DeleteProject while apps still reference
// the project. There is no DB-level foreign key on apps.project_id (see
// migration 0004), so this guard is what keeps the reference sound.
var ErrProjectNotEmpty = errors.New("store: project still has apps")

// CreateProject inserts a project and returns it with ID and CreatedAt set.
func (s *Store) CreateProject(ctx context.Context, p core.Project) (core.Project, error) {
	p.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, description, created_at) VALUES (?, ?, ?)`,
		p.Name, p.Description, fmtTime(p.CreatedAt))
	if err != nil {
		return core.Project{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Project{}, err
	}
	p.ID = id
	return p, nil
}

func (s *Store) GetProject(ctx context.Context, id int64) (core.Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, created_at FROM projects WHERE id = ?`, id)
	return scanProject(row)
}

func (s *Store) ListProjects(ctx context.Context) ([]core.Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, created_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []core.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// UpdateProject renames a project and replaces its description.
func (s *Store) UpdateProject(ctx context.Context, id int64, name, description string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, description = ? WHERE id = ?`, name, description, id)
	return err
}

// DeleteProject removes an empty project; a project with apps is refused with
// ErrProjectNotEmpty. Count and delete run in one transaction so an app
// created concurrently cannot slip into a just-deleted project.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM apps WHERE project_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrProjectNotEmpty
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CountAppsByProject returns app counts keyed by project id (projects with no
// apps are absent from the map).
func (s *Store) CountAppsByProject(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, COUNT(*) FROM apps GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		counts[id] = n
	}
	return counts, rows.Err()
}

func scanProject(row scanner) (core.Project, error) {
	var (
		p         core.Project
		createdAt string
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &createdAt); err != nil {
		return core.Project{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return core.Project{}, err
	}
	p.CreatedAt = t
	return p, nil
}
