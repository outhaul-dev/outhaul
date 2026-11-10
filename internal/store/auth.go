package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// HasUser reports whether an admin user exists (drives the first-boot flow).
func (s *Store) HasUser(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CreateUser inserts a system admin with a password. It is the first-boot path.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (core.User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at, role) VALUES (?, ?, ?, ?)`,
		username, passwordHash, fmtTime(now), string(core.RoleSystemAdmin))
	if err != nil {
		return core.User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.User{}, err
	}
	return s.GetUser(ctx, id)
}

// GetUserByName finds a password account; SSO accounts are never found by name.
func (s *Store) GetUserByName(ctx context.Context, username string) (core.User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = ? AND sign_in = 'password'`, username))
}

// GetUser returns the user with the given id.
func (s *Store) GetUser(ctx context.Context, id int64) (core.User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UpdateUserPassword sets a new password hash for the user and, in the same
// transaction, revokes every one of their sessions except keepSession (the
// one making the change; "" revokes all).
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash, keepSession string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requirePasswordAccount(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ? AND sign_in = 'password'`, passwordHash, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token <> ?`, id, keepSession); err != nil {
		return err
	}
	// The requirePasswordAccount check above makes this a password account.
	if err := deleteGateSessionsFor(ctx, tx, core.User{ID: id}.GateSubject()); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateSession stores a session. CreatedAt defaults to now when zero.
func (s *Store) CreateSession(ctx context.Context, sess core.Session) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.Token, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt))
	return err
}

// ErrPasswordChanged means a password sign-in's account no longer has the
// password hash that was checked, or is gone, so no session was made.
var ErrPasswordChanged = errors.New("store: the account's password changed during sign-in")

// CreatePasswordSession stores sess for a password sign-in. Checking the
// password is slow and happens outside any transaction, so it re-reads the
// account in the same transaction as the insert: if its hash is no longer
// verifiedHash (a reset or change committed meanwhile), is blank, or the
// account is gone or isn't a password account, it returns ErrPasswordChanged
// and stores nothing. CreatedAt defaults to now when zero.
func (s *Store) CreatePasswordSession(ctx context.Context, sess core.Session, verifiedHash string) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := checkedPasswordAccount(ctx, tx, sess.UserID, verifiedHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.Token, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// CreatePasswordGateSession stores a gateway session for a password sign-in
// by account userID, under the hash of rawToken, expiring at expiresAt. Like
// CreatePasswordSession it re-reads the account in the same transaction as
// the insert, and returns ErrPasswordChanged, storing nothing, if its hash is
// no longer verifiedHash, is blank, or the account is gone or isn't a
// password account. The session's subject is the account's GateSubject and
// its display name the account's, as they are in that transaction. Otherwise
// it is CreateGateSession, refusals and clean-up included.
func (s *Store) CreatePasswordGateSession(ctx context.Context, rawToken string, userID int64, verifiedHash string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := checkedPasswordAccount(ctx, tx, userID, verifiedHash)
	if err != nil {
		return err
	}
	if err := insertGateSession(ctx, tx, rawToken, core.GateSession{
		Subject: u.GateSubject(), Kind: core.SignInPassword, Display: u.Display(), ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// checkedPasswordAccount reads password account id inside tx and returns it
// if its hash is still verifiedHash. It returns ErrPasswordChanged when the
// hash differs or is blank, or the account is gone or isn't a password
// account.
func checkedPasswordAccount(ctx context.Context, tx *sql.Tx, id int64, verifiedHash string) (core.User, error) {
	u, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ? AND sign_in = 'password'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, ErrPasswordChanged
	}
	if err != nil {
		return core.User{}, err
	}
	if u.PasswordHash == "" || u.PasswordHash != verifiedHash {
		return core.User{}, ErrPasswordChanged
	}
	return u, nil
}

// GetSession returns a non-expired session, or sql.ErrNoRows if missing/expired.
func (s *Store) GetSession(ctx context.Context, token string) (core.Session, error) {
	var (
		sess      core.Session
		createdAt string
		expiresAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT token, user_id, created_at, expires_at FROM sessions WHERE token = ?`, token).
		Scan(&sess.Token, &sess.UserID, &createdAt, &expiresAt)
	if err != nil {
		return core.Session{}, err
	}
	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return core.Session{}, err
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return core.Session{}, err
	}
	if time.Now().After(sess.ExpiresAt) {
		return core.Session{}, sql.ErrNoRows
	}
	return sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}
