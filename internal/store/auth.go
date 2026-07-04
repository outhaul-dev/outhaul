package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/james-smart/outhaul/internal/core"
)

// HasUser reports whether an admin user exists (drives the first-boot flow).
func (s *Store) HasUser(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// CreateUser inserts the admin user.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (core.User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, passwordHash, fmtTime(now))
	if err != nil {
		return core.User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.User{}, err
	}
	return core.User{ID: id, Username: username, PasswordHash: passwordHash, CreatedAt: now}, nil
}

func (s *Store) GetUserByName(ctx context.Context, username string) (core.User, error) {
	var (
		u         core.User
		createdAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt)
	if err != nil {
		return core.User{}, err
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return core.User{}, err
	}
	return u, nil
}

// GetUser returns the user with the given id.
func (s *Store) GetUser(ctx context.Context, id int64) (core.User, error) {
	var u core.User
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAt)
	if err != nil {
		return core.User{}, err
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return core.User{}, err
	}
	return u, nil
}

// UpdateUserPassword sets a new password hash for the user.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	return err
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
