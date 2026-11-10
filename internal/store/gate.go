package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// hashToken is the SHA-256 hex of a raw token or code. Setup links, gateway
// sessions, route sessions and one-time codes are stored only in this form.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// deleteGateSessionsFor ends the gateway sessions of subject. Their route
// sessions and codes cascade. An empty subject deletes nothing.
func deleteGateSessionsFor(ctx context.Context, q execer, subject string) error {
	if subject == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `DELETE FROM gate_sessions WHERE subject = ?`, subject)
	return err
}

// deleteGateSessionsOfKind ends every gateway session of one sign-in kind.
// An empty kind deletes nothing.
func deleteGateSessionsOfKind(ctx context.Context, q execer, kind string) error {
	if kind == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `DELETE FROM gate_sessions WHERE kind = ?`, kind)
	return err
}

// accountGateSubject returns the gateway subject of account id, or "" if it
// does not exist.
func accountGateSubject(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return u.GateSubject(), nil
}

func normHost(host string) string { return strings.ToLower(host) }

// CreateGateSession stores a gateway session under the hash of rawToken. It
// also deletes what has expired: gateway sessions (their route sessions and
// codes go with them), and route sessions and codes past their own expiry.
func (s *Store) CreateGateSession(ctx context.Context, rawToken string, sess core.GateSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	// Timestamps are RFC3339 strings of varying precision, so expiry is
	// compared in Go.
	for _, t := range []struct{ table, col string }{
		{"gate_sessions", "token_hash"}, {"route_sessions", "token_hash"}, {"gate_codes", "code_hash"},
	} {
		if err := deleteExpired(ctx, tx, t.table, t.col, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gate_sessions (token_hash, subject, kind, display, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(rawToken), sess.Subject, sess.Kind, sess.Display, fmtTime(now), fmtTime(sess.ExpiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteExpired(ctx context.Context, tx *sql.Tx, table, keyCol string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+keyCol+`, expires_at FROM `+table)
	if err != nil {
		return err
	}
	var dead []string
	for rows.Next() {
		var key, exp string
		if err := rows.Scan(&key, &exp); err != nil {
			rows.Close()
			return err
		}
		if t, err := parseTime(exp); err != nil || !now.Before(t) {
			dead = append(dead, key)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range dead {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+keyCol+` = ?`, k); err != nil {
			return err
		}
	}
	return nil
}

// GetGateSession returns a live gateway session, or sql.ErrNoRows when it is
// unknown or has expired.
func (s *Store) GetGateSession(ctx context.Context, rawToken string) (core.GateSession, error) {
	var (
		g   core.GateSession
		exp string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT subject, kind, display, expires_at FROM gate_sessions WHERE token_hash = ?`,
		hashToken(rawToken)).Scan(&g.Subject, &g.Kind, &g.Display, &exp)
	if err != nil {
		return core.GateSession{}, err
	}
	if g.ExpiresAt, err = parseTime(exp); err != nil {
		return core.GateSession{}, err
	}
	if !time.Now().Before(g.ExpiresAt) {
		return core.GateSession{}, sql.ErrNoRows
	}
	return g, nil
}

// DeleteGateSession ends one gateway session and, with it, its route sessions.
func (s *Store) DeleteGateSession(ctx context.Context, rawToken string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM gate_sessions WHERE token_hash = ?`, hashToken(rawToken))
	return err
}

// DeleteGateSessionsForSubject ends every gateway session of subject.
func (s *Store) DeleteGateSessionsForSubject(ctx context.Context, subject string) error {
	return deleteGateSessionsFor(ctx, s.db, subject)
}

// DeleteGateSessionsOfKind ends every gateway session of one sign-in kind.
func (s *Store) DeleteGateSessionsOfKind(ctx context.Context, kind string) error {
	return deleteGateSessionsOfKind(ctx, s.db, kind)
}

// CreateGateCode stores a one-time code that carries the gateway session to
// host. Only the hash of the code is kept.
func (s *Store) CreateGateCode(ctx context.Context, rawCode, rawGateToken, host string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gate_codes (code_hash, gate_token_hash, host, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(rawCode), hashToken(rawGateToken), normHost(host), fmtTime(expiresAt))
	return err
}

// RedeemGateCode turns a code into a route session for host, in one
// transaction: it checks the code, deletes it and stores the route session
// under the hash of rawRouteToken. It returns sql.ErrNoRows when the code is
// unknown, has expired, is for another host (the code is kept, so it still
// works on its own host), or its gateway session is gone or has expired. The
// route session ends at routeExpires or when its gateway session does,
// whichever is first.
func (s *Store) RedeemGateCode(ctx context.Context, rawCode, host, rawRouteToken string, routeExpires time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var (
		gateHash, codeHost, codeExp, display, gateExp string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT c.gate_token_hash, c.host, c.expires_at, g.display, g.expires_at
		   FROM gate_codes c JOIN gate_sessions g ON g.token_hash = c.gate_token_hash
		  WHERE c.code_hash = ?`, hashToken(rawCode)).Scan(&gateHash, &codeHost, &codeExp, &display, &gateExp)
	if err != nil {
		return "", err
	}
	if codeHost != normHost(host) {
		return "", sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gate_codes WHERE code_hash = ?`, hashToken(rawCode)); err != nil {
		return "", err
	}
	now := time.Now()
	ce, err := parseTime(codeExp)
	if err != nil {
		return "", err
	}
	ge, err := parseTime(gateExp)
	if err != nil {
		return "", err
	}
	if !now.Before(ce) || !now.Before(ge) {
		// The code is dead either way; commit its deletion.
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return "", sql.ErrNoRows
	}
	if ge.Before(routeExpires) {
		routeExpires = ge
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO route_sessions (token_hash, gate_token_hash, host, display, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(rawRouteToken), gateHash, normHost(host), display, fmtTime(routeExpires)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return display, nil
}

// RouteSessionUser returns the display name of the route session for host. It
// returns sql.ErrNoRows when the token is unknown, expired, for another host,
// or its gateway session is gone or has expired. It is one indexed read and
// writes nothing: it runs on every request to a protected route.
func (s *Store) RouteSessionUser(ctx context.Context, rawToken, host string) (string, error) {
	var display, routeExp, gateExp string
	err := s.db.QueryRowContext(ctx,
		`SELECT r.display, r.expires_at, g.expires_at
		   FROM route_sessions r JOIN gate_sessions g ON g.token_hash = r.gate_token_hash
		  WHERE r.token_hash = ? AND r.host = ?`, hashToken(rawToken), normHost(host)).Scan(&display, &routeExp, &gateExp)
	if err != nil {
		return "", err
	}
	now := time.Now()
	for _, e := range []string{routeExp, gateExp} {
		t, err := parseTime(e)
		if err != nil {
			return "", err
		}
		if !now.Before(t) {
			return "", sql.ErrNoRows
		}
	}
	return display, nil
}
