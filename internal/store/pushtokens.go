package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

var (
	ErrPushTokenLabel      = errors.New("push token: label must be 1 to 64 characters")
	ErrPushTokenLabelTaken = errors.New("push token: label already used")
	ErrPushTokenLimit      = errors.New("push token: account has 25 tokens")
)

const pushTokenCols = `id, user_id, label, created_at, expires_at, last_used_at, last_used_ip`

// pushTokenTouchEvery is the least gap between two recorded uses from one address.
const pushTokenTouchEvery = time.Minute

// CreatePushToken makes a token for the account and returns it once. Only
// its SHA-256 is stored.
func (s *Store) CreatePushToken(ctx context.Context, userID int64, label string, now, expires time.Time) (raw string, t core.PushToken, err error) {
	label, ok := core.CleanPushTokenLabel(label)
	if !ok {
		return "", core.PushToken{}, ErrPushTokenLabel
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", core.PushToken{}, err
	}
	raw = core.PushTokenPrefix + hex.EncodeToString(buf)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", core.PushToken{}, err
	}
	defer tx.Rollback()
	if err := prunePushTokens(ctx, tx, now); err != nil {
		return "", core.PushToken{}, err
	}
	var n int
	var taken bool
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(label = ?), 0) FROM push_tokens WHERE user_id = ?`,
		label, userID).Scan(&n, &taken); err != nil {
		return "", core.PushToken{}, err
	}
	if n >= core.MaxPushTokensPerUser {
		return "", core.PushToken{}, ErrPushTokenLimit
	}
	if taken {
		return "", core.PushToken{}, ErrPushTokenLabelTaken
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO push_tokens (user_id, label, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		userID, label, hashToken(raw), fmtTime(now), fmtTime(expires))
	if err != nil {
		return "", core.PushToken{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", core.PushToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", core.PushToken{}, err
	}
	return raw, core.PushToken{
		ID: id, UserID: userID, Label: label,
		CreatedAt: now.UTC(), ExpiresAt: expires.UTC(),
	}, nil
}

// FindPushToken returns the row for raw, expired or not. A value of the wrong
// shape is not looked up.
func (s *Store) FindPushToken(ctx context.Context, raw string) (core.PushToken, bool, error) {
	if !core.ValidPushTokenShape(raw) {
		return core.PushToken{}, false, nil
	}
	want := hashToken(raw)
	var stored string
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash FROM push_tokens WHERE token_hash = ?`, want).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PushToken{}, false, nil
	}
	if err != nil {
		return core.PushToken{}, false, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(want)) != 1 {
		return core.PushToken{}, false, nil
	}
	t, err := s.GetPushToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PushToken{}, false, nil
	}
	if err != nil {
		return core.PushToken{}, false, err
	}
	return t, true, nil
}

// GetPushToken returns sql.ErrNoRows when the row is gone.
func (s *Store) GetPushToken(ctx context.Context, id int64) (core.PushToken, error) {
	return scanPushToken(s.db.QueryRowContext(ctx,
		`SELECT `+pushTokenCols+` FROM push_tokens WHERE id = ?`, id))
}

// ListPushTokensForUser returns one account's tokens, newest first, after
// deleting those expired for longer than core.PushTokenKeepExpired.
func (s *Store) ListPushTokensForUser(ctx context.Context, userID int64, now time.Time) ([]core.PushToken, error) {
	if err := prunePushTokens(ctx, s.db, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pushTokenCols+` FROM push_tokens WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.PushToken
	for rows.Next() {
		t, err := scanPushToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListPushTokens returns every account's tokens with the owner's display
// name, newest first, after the same pruning.
func (s *Store) ListPushTokens(ctx context.Context, now time.Time) ([]core.PushTokenRow, error) {
	if err := prunePushTokens(ctx, s.db, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.user_id, t.label, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip,
		        u.username, u.sign_in, u.email
		 FROM push_tokens t JOIN users u ON u.id = t.user_id
		 ORDER BY t.created_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.PushTokenRow
	for rows.Next() {
		var r core.PushTokenRow
		var created, expires string
		var lastUsed sql.NullString
		var u core.User
		if err := rows.Scan(&r.ID, &r.UserID, &r.Label, &created, &expires, &lastUsed, &r.LastUsedIP,
			&u.Username, &u.SignIn, &u.Email); err != nil {
			return nil, err
		}
		if err := fillPushToken(&r.PushToken, created, expires, lastUsed); err != nil {
			return nil, err
		}
		r.Owner = u.Display()
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeletePushTokenForUser deletes nothing when the id is another account's.
func (s *Store) DeletePushTokenForUser(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// TouchPushToken records a use, at most once a minute unless ip changed.
func (s *Store) TouchPushToken(ctx context.Context, id int64, at time.Time, ip string) error {
	t, err := s.GetPushToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.LastUsedAt != nil && t.LastUsedIP == ip && at.Sub(*t.LastUsedAt) < pushTokenTouchEvery {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE push_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, fmtTime(at), ip, id)
	return err
}

// prunePushTokens deletes tokens that expired more than
// core.PushTokenKeepExpired before now. The times are compared as times, not
// as text, because RFC 3339 with fractional seconds doesn't sort as text.
func prunePushTokens(ctx context.Context, q interface {
	execer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, now time.Time) error {
	rows, err := q.QueryContext(ctx, `SELECT id, expires_at FROM push_tokens`)
	if err != nil {
		return err
	}
	cutoff := now.Add(-core.PushTokenKeepExpired)
	var stale []int64
	for rows.Next() {
		var id int64
		var expires string
		if err := rows.Scan(&id, &expires); err != nil {
			rows.Close()
			return err
		}
		e, err := parseTime(expires)
		if err != nil {
			rows.Close()
			return err
		}
		if e.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range stale {
		if _, err := q.ExecContext(ctx, `DELETE FROM push_tokens WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func scanPushToken(row scanner) (core.PushToken, error) {
	var t core.PushToken
	var created, expires string
	var lastUsed sql.NullString
	if err := row.Scan(&t.ID, &t.UserID, &t.Label, &created, &expires, &lastUsed, &t.LastUsedIP); err != nil {
		return core.PushToken{}, err
	}
	if err := fillPushToken(&t, created, expires, lastUsed); err != nil {
		return core.PushToken{}, err
	}
	return t, nil
}

func fillPushToken(t *core.PushToken, created, expires string, lastUsed sql.NullString) error {
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return err
	}
	if t.ExpiresAt, err = parseTime(expires); err != nil {
		return err
	}
	t.LastUsedAt, err = scanTime(lastUsed)
	return err
}
