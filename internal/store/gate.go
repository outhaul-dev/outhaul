package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
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

// errGateArg is returned for an empty or unknown value in a write.
func errGateArg(what string) error { return errors.New("store: gate: " + what) }

// deleteExpiredGate removes gateway sessions, route sessions and codes whose
// expiry is at or before now. julianday reads the stored RFC3339 text with
// millisecond precision, so rows within a millisecond of now may go either way;
// the read paths compare exactly, in Go.
func deleteExpiredGate(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for _, table := range []string{"gate_sessions", "route_sessions", "gate_codes"} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE julianday(expires_at) <= julianday(?)`, fmtTime(now)); err != nil {
			return err
		}
	}
	return nil
}

// CreateGateSession stores a gateway session under the hash of rawToken. It
// also deletes what has expired: gateway sessions (their route sessions and
// codes go with them), and route sessions and codes past their own expiry.
// It refuses an empty token, subject or display, and a kind other than
// core.SignInPassword or core.SignInSSO.
func (s *Store) CreateGateSession(ctx context.Context, rawToken string, sess core.GateSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertGateSession(ctx, tx, rawToken, sess); err != nil {
		return err
	}
	return tx.Commit()
}

// insertGateSession is CreateGateSession inside an open transaction.
func insertGateSession(ctx context.Context, tx *sql.Tx, rawToken string, sess core.GateSession) error {
	switch {
	case rawToken == "":
		return errGateArg("a session needs a token")
	case sess.Subject == "":
		return errGateArg("a session needs a subject")
	case sess.Display == "":
		return errGateArg("a session needs a display name")
	case sess.Kind != core.SignInPassword && sess.Kind != core.SignInSSO:
		return errGateArg("unknown sign-in kind " + sess.Kind)
	}
	now := time.Now().UTC()
	if err := deleteExpiredGate(ctx, tx, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO gate_sessions (token_hash, subject, kind, display, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(rawToken), sess.Subject, sess.Kind, sess.Display, fmtTime(now), fmtTime(sess.ExpiresAt))
	return err
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
// host, in the browser whose pending cookie hashes to nonceHash. Only the hash
// of the code is kept. The session's earlier unredeemed codes for the same
// host are deleted, so one signed-in person can't pile codes up. An empty
// rawCode or nonceHash is refused with an error that isn't sql.ErrNoRows.
func (s *Store) CreateGateCode(ctx context.Context, rawCode, rawGateToken, host, nonceHash string, expiresAt time.Time) error {
	if rawCode == "" {
		return errGateArg("a code can't be empty")
	}
	if nonceHash == "" {
		return errGateArg("a code needs the browser's nonce")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM gate_codes WHERE gate_token_hash = ? AND host = ?`,
		hashToken(rawGateToken), normHost(host)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO gate_codes (code_hash, gate_token_hash, host, nonce_hash, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(rawCode), hashToken(rawGateToken), normHost(host), nonceHash, fmtTime(expiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// RedeemGateCode turns a code into a route session for host, in one
// transaction: it checks the code, deletes it and stores the route session
// under the hash of rawRouteToken. It returns sql.ErrNoRows when the code is
// unknown, has expired, is for another host, or was minted for another
// browser (nonceHash, empty included, isn't the one it was created with), or
// its gateway session is gone or has expired. A code refused for its host or
// its browser is kept, so it still works where it belongs. An empty rawCode or
// rawRouteToken is refused with an error that isn't sql.ErrNoRows. The route
// session ends at routeExpires or when its gateway session does, whichever is
// first.
func (s *Store) RedeemGateCode(ctx context.Context, rawCode, host, nonceHash, rawRouteToken string, routeExpires time.Time) (string, error) {
	if rawCode == "" || rawRouteToken == "" {
		return "", errGateArg("a code and a route token can't be empty")
	}
	if nonceHash == "" {
		return "", sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var (
		gateHash, codeHost, codeNonce, codeExp, display, gateExp string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT c.gate_token_hash, c.host, c.nonce_hash, c.expires_at, g.display, g.expires_at
		   FROM gate_codes c JOIN gate_sessions g ON g.token_hash = c.gate_token_hash
		  WHERE c.code_hash = ?`, hashToken(rawCode)).Scan(&gateHash, &codeHost, &codeNonce, &codeExp, &display, &gateExp)
	if err != nil {
		return "", err
	}
	if codeHost != normHost(host) {
		return "", sql.ErrNoRows
	}
	if subtle.ConstantTimeCompare([]byte(codeNonce), []byte(nonceHash)) != 1 {
		return "", sql.ErrNoRows
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM gate_codes WHERE code_hash = ?`, hashToken(rawCode))
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", err
	} else if n != 1 {
		return "", sql.ErrNoRows
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
