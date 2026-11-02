package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// ErrLastPasswordAdmin is returned when a change would leave no password
// account that is a system admin, which would lock everyone out of recovery.
var ErrLastPasswordAdmin = errors.New("store: this is the last password account that is a system admin")

const userCols = `id, username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email, last_sign_in_at`

func scanUser(row scanner) (core.User, error) {
	var (
		u         core.User
		createdAt string
		role      string
		lastIn    sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt, &role, &u.SignIn,
		&u.SSOIssuer, &u.SSOSubject, &u.Email, &lastIn); err != nil {
		return core.User{}, err
	}
	u.Role = core.Role(role)
	var err error
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return core.User{}, err
	}
	if u.LastSignInAt, err = scanTime(lastIn); err != nil {
		return core.User{}, err
	}
	return u, nil
}

// CreatePasswordAccount adds a password account with no password yet, so it
// cannot sign in until its owner sets one through a setup link.
func (s *Store) CreatePasswordAccount(ctx context.Context, username string, role core.Role) (core.User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at, role) VALUES (?, '', ?, ?)`,
		username, fmtTime(time.Now().UTC()), string(role))
	if err != nil {
		return core.User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.User{}, err
	}
	return s.GetUser(ctx, id)
}

// ListUsers returns every account: SSO accounts with no role first (they are
// waiting for an admin), then by display name.
func (s *Store) ListUsers(ctx context.Context) ([]core.UserRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userCols+`, (SELECT COUNT(*) FROM project_members m WHERE m.user_id = users.id)
		 FROM users
		 ORDER BY (sign_in = 'sso' AND role = 'none') DESC,
		          LOWER(CASE WHEN sign_in = 'sso' AND email <> '' THEN email ELSE username END), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.UserRow
	for rows.Next() {
		var (
			r         core.UserRow
			createdAt string
			role      string
			lastIn    sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Username, &r.PasswordHash, &createdAt, &role, &r.SignIn,
			&r.SSOIssuer, &r.SSOSubject, &r.Email, &lastIn, &r.ProjectCount); err != nil {
			return nil, err
		}
		r.Role = core.Role(role)
		if r.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if r.LastSignInAt, err = scanTime(lastIn); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetUserRole changes an account's role. It refuses to demote the last
// password account that is a system admin.
func (s *Store) SetUserRole(ctx context.Context, id int64, role core.Role) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if role != core.RoleSystemAdmin {
		if err := guardLastPasswordAdmin(ctx, tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, string(role), id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUser removes an account with its sessions and push keys (project
// memberships and setup links cascade). It refuses to remove the last
// password account that is a system admin.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := guardLastPasswordAdmin(ctx, tx, id); err != nil {
		return err
	}
	// sessions.user_id has no cascade.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// guardLastPasswordAdmin returns ErrLastPasswordAdmin when id is a password
// system admin and no other one exists. The change that calls it removes the
// admin's standing, so it must run in the same transaction as that change.
func guardLastPasswordAdmin(ctx context.Context, tx *sql.Tx, id int64) error {
	var isAdmin bool
	err := tx.QueryRowContext(ctx,
		`SELECT sign_in = 'password' AND role = 'system_admin' FROM users WHERE id = ?`, id).Scan(&isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || !isAdmin {
		return err
	}
	var others int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE sign_in = 'password' AND role = 'system_admin' AND id <> ?`, id).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastPasswordAdmin
	}
	return nil
}

// EndUserSessions signs the account out everywhere.
func (s *Store) EndUserSessions(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
	return err
}

// TouchUserSignIn stamps last_sign_in_at = now.
func (s *Store) TouchUserSignIn(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_sign_in_at = ? WHERE id = ?`, fmtTime(time.Now().UTC()), id)
	return err
}
