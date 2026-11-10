package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// ErrLastPasswordAdmin is returned when a change would leave no password
// account that is a system admin, which would lock everyone out of recovery.
var ErrLastPasswordAdmin = errors.New("store: this is the last password account that is a system admin")

// ErrNotPasswordAccount is returned when a password operation (a setup link,
// a reset, a new password) is asked of an account that signs in with SSO.
var ErrNotPasswordAccount = errors.New("store: this account does not sign in with a password")

// requirePasswordAccount returns sql.ErrNoRows for a missing account and
// ErrNotPasswordAccount for one that isn't a password account.
func requirePasswordAccount(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) error {
	var signIn string
	if err := q.QueryRowContext(ctx, `SELECT sign_in FROM users WHERE id = ?`, id).Scan(&signIn); err != nil {
		return err
	}
	if signIn != string(core.SignInPassword) {
		return ErrNotPasswordAccount
	}
	return nil
}

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

// FindOrCreateSSOUser returns the SSO account for issuer and subject, creating
// it with no role on the first sign-in. A later sign-in keeps its id and role
// and refreshes its email. Email never selects an account, and a password
// account is never returned or changed. The username is
// "sso:" + issuer + "|" + subject, which the Users page never shows.
//
// It is one transaction. If two first sign-ins race, the insert of the second
// does nothing and it reads the account the first created.
func (s *Store) FindOrCreateSSOUser(ctx context.Context, issuer, subject, email string) (core.User, error) {
	if issuer == "" || subject == "" || email == "" {
		return core.User{}, errors.New("store: an SSO account needs an issuer, a subject and an email")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.User{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at, role, sign_in, sso_issuer, sso_subject, email)
		 VALUES (?, '', ?, ?, 'sso', ?, ?, ?)
		 ON CONFLICT DO NOTHING`,
		"sso:"+issuer+"|"+subject, fmtTime(time.Now().UTC()), string(core.RoleNone), issuer, subject, email); err != nil {
		return core.User{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET email = ? WHERE sign_in = 'sso' AND sso_issuer = ? AND sso_subject = ?`,
		email, issuer, subject); err != nil {
		return core.User{}, err
	}
	// Not found means the insert did nothing because a password account holds
	// the username: refuse rather than touch it.
	u, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE sign_in = 'sso' AND sso_issuer = ? AND sso_subject = ?`,
		issuer, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, fmt.Errorf("store: the username for this SSO account is taken by a password account")
	}
	if err != nil {
		return core.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.User{}, err
	}
	return u, nil
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
		var r core.UserRow
		if r.User, err = scanUser(userRowScanner{rows, &r.ProjectCount}); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// userRowScanner lets scanUser read the user columns of a ListUsers row and
// pick up the trailing project count in the same Scan.
type userRowScanner struct {
	rows  *sql.Rows
	count *int
}

func (u userRowScanner) Scan(dest ...any) error {
	return u.rows.Scan(append(dest, u.count)...)
}

// SetUserRole changes an account's role. It refuses to demote the last
// password account that is a system admin.
func (s *Store) SetUserRole(ctx context.Context, id int64, role core.Role) error {
	if _, ok := core.ParseRole(string(role)); !ok {
		return fmt.Errorf("store: unknown role %q", role)
	}
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
	res, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, string(role), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
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
// system admin and no other one exists who can sign in (has a password set). The change that calls it removes the
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
		`SELECT COUNT(*) FROM users WHERE sign_in = 'password' AND role = 'system_admin' AND password_hash <> '' AND id <> ?`, id).Scan(&others); err != nil {
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

// AddProjectMember puts an account on a project. Adding twice is fine.
func (s *Store) AddProjectMember(ctx context.Context, projectID, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO project_members (project_id, user_id, created_at) VALUES (?, ?, ?)`,
		projectID, userID, fmtTime(time.Now().UTC()))
	return err
}

// RemoveProjectMember takes an account off a project.
func (s *Store) RemoveProjectMember(ctx context.Context, projectID, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID)
	return err
}

// IsProjectMember reports whether the account is on the project.
func (s *Store) IsProjectMember(ctx context.Context, projectID, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) queryUsers(ctx context.Context, query string, args ...any) ([]core.User, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListProjectMembers returns the accounts on a project. System admins are
// left out: they reach every project anyway, even if they joined before being
// promoted.
func (s *Store) ListProjectMembers(ctx context.Context, projectID int64) ([]core.User, error) {
	return s.queryUsers(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE role <> 'system_admin' AND id IN (SELECT user_id FROM project_members WHERE project_id = ?)
		 ORDER BY LOWER(CASE WHEN sign_in = 'sso' AND email <> '' THEN email ELSE username END), id`, projectID)
}

// ListShareableUsers returns the accounts with a project role who are not yet
// on the project.
func (s *Store) ListShareableUsers(ctx context.Context, projectID int64) ([]core.User, error) {
	return s.queryUsers(ctx,
		`SELECT `+userCols+` FROM users
		 WHERE role IN ('project_viewer', 'project_member', 'project_admin')
		   AND id NOT IN (SELECT user_id FROM project_members WHERE project_id = ?)
		 ORDER BY LOWER(CASE WHEN sign_in = 'sso' AND email <> '' THEN email ELSE username END), id`, projectID)
}

// ProjectIDsForUser returns the ids of the projects the account is on.
func (s *Store) ProjectIDsForUser(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id FROM project_members WHERE user_id = ? ORDER BY project_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func hashSetupToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// replaceSetupLink drops the account's earlier links and stores a new one.
// Only the SHA-256 of the token is kept.
func replaceSetupLink(ctx context.Context, tx *sql.Tx, userID int64, rawToken string, expiresAt time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_links WHERE user_id = ?`, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO setup_links (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		hashSetupToken(rawToken), userID, fmtTime(expiresAt), fmtTime(time.Now().UTC()))
	return err
}

// CreateSetupLink stores a one-time link for the account, replacing any
// earlier one.
func (s *Store) CreateSetupLink(ctx context.Context, userID int64, rawToken string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requirePasswordAccount(ctx, tx, userID); err != nil {
		return err
	}
	if err := replaceSetupLink(ctx, tx, userID, rawToken, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

// setupLinkUser finds the account a live link belongs to; sql.ErrNoRows when
// the link is unknown or expired.
func setupLinkUser(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, rawToken string) (core.User, error) {
	var userID int64
	var expires string
	err := q.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM setup_links WHERE token_hash = ?`, hashSetupToken(rawToken)).Scan(&userID, &expires)
	if err != nil {
		return core.User{}, err
	}
	exp, err := parseTime(expires)
	if err != nil {
		return core.User{}, err
	}
	if !time.Now().Before(exp) {
		return core.User{}, sql.ErrNoRows
	}
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, userID))
	if err != nil {
		return core.User{}, err
	}
	if u.SignIn != core.SignInPassword {
		return core.User{}, sql.ErrNoRows
	}
	return u, nil
}

// SetupLinkUser returns the account a setup link is for. It returns
// sql.ErrNoRows when the link is unknown or has expired.
func (s *Store) SetupLinkUser(ctx context.Context, rawToken string) (core.User, error) {
	return setupLinkUser(ctx, s.db, rawToken)
}

// CompleteSetupLink uses a link: in one transaction it checks the link, sets
// the password hash, deletes the link and ends the account's sessions. A used
// or expired link gives sql.ErrNoRows.
func (s *Store) CompleteSetupLink(ctx context.Context, rawToken, passwordHash string) (core.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.User{}, err
	}
	defer tx.Rollback()
	u, err := setupLinkUser(ctx, tx, rawToken)
	if err != nil {
		return core.User{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ? AND sign_in = 'password'`, passwordHash, u.ID); err != nil {
		return core.User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_links WHERE user_id = ?`, u.ID); err != nil {
		return core.User{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.ID); err != nil {
		return core.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.User{}, err
	}
	u.PasswordHash = passwordHash
	return u, nil
}

// ResetUserPassword stops the account's password working at once: in one
// transaction it applies the last-admin guard, blanks the hash, ends the
// sessions and issues a new setup link in place of any earlier one.
func (s *Store) ResetUserPassword(ctx context.Context, id int64, rawToken string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requirePasswordAccount(ctx, tx, id); err != nil {
		return err
	}
	if err := guardLastPasswordAdmin(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = '' WHERE id = ? AND sign_in = 'password'`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	if err := replaceSetupLink(ctx, tx, id, rawToken, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}
