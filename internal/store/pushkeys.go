package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

const pushKeyCols = `id, label, fingerprint, public_key, created_at, last_used_at, user_id`

// AddPushKey inserts a push key and returns it with ID and CreatedAt set. A
// duplicate fingerprint violates UNIQUE and returns an error.
func (s *Store) AddPushKey(ctx context.Context, pk core.PushKey) (core.PushKey, error) {
	pk.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO push_keys (label, fingerprint, public_key, created_at, last_used_at, user_id)
		 VALUES (?, ?, ?, ?, NULL, ?)`,
		pk.Label, pk.Fingerprint, pk.PublicKey, fmtTime(pk.CreatedAt), nullID(pk.UserID))
	if err != nil {
		return core.PushKey{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.PushKey{}, err
	}
	pk.ID = id
	pk.LastUsedAt = nil
	return pk, nil
}

// ListPushKeysForUser returns one account's keys, newest first.
func (s *Store) ListPushKeysForUser(ctx context.Context, userID int64) ([]core.PushKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pushKeyCols+` FROM push_keys WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.PushKey
	for rows.Next() {
		pk, err := scanPushKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// FindPushKeyByFingerprint looks up a single key by its SHA256 fingerprint.
// The bool is false (with a nil error) when no key matches.
func (s *Store) FindPushKeyByFingerprint(ctx context.Context, fingerprint string) (core.PushKey, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pushKeyCols+` FROM push_keys WHERE fingerprint = ?`, fingerprint)
	pk, err := scanPushKey(row)
	if err == sql.ErrNoRows {
		return core.PushKey{}, false, nil
	}
	if err != nil {
		return core.PushKey{}, false, err
	}
	return pk, true, nil
}

// TouchPushKey stamps last_used_at = now for the given key.
func (s *Store) TouchPushKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE push_keys SET last_used_at = ? WHERE id = ?`, fmtTime(time.Now().UTC()), id)
	return err
}

// DeletePushKeyForUser removes a key only if the account owns it.
func (s *Store) DeletePushKeyForUser(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_keys WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// nullID stores 0 as NULL.
func nullID(id int64) sql.NullInt64 {
	return sql.NullInt64{Int64: id, Valid: id != 0}
}

func scanPushKey(row scanner) (core.PushKey, error) {
	var pk core.PushKey
	var created string
	var lastUsed sql.NullString
	var owner sql.NullInt64
	if err := row.Scan(&pk.ID, &pk.Label, &pk.Fingerprint, &pk.PublicKey, &created, &lastUsed, &owner); err != nil {
		return core.PushKey{}, err
	}
	pk.UserID = owner.Int64
	var err error
	if pk.CreatedAt, err = parseTime(created); err != nil { // created is NOT NULL
		return core.PushKey{}, err
	}
	if pk.LastUsedAt, err = scanTime(lastUsed); err != nil {
		return core.PushKey{}, err
	}
	return pk, nil
}
