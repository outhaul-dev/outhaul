package store

import (
	"context"
	"fmt"

	"github.com/outhaul-dev/outhaul/internal/core"
)

// Settings keys for the SSO provider. The client secret is sealed.
const (
	ssoIssuerKey   = "sso_issuer"
	ssoClientIDKey = "sso_client_id"
	ssoSecretKey   = "sso_client_secret"
)

// SSOSettings is the OIDC provider a system admin connected.
type SSOSettings struct {
	Issuer, ClientID, ClientSecret string
}

// GetSSOSettings returns the stored provider settings with the secret
// decrypted. ok is false (nil error) when none are stored.
func (s *Store) GetSSOSettings(ctx context.Context) (SSOSettings, bool, error) {
	issuer, ok, err := s.GetSetting(ctx, ssoIssuerKey)
	if err != nil || !ok {
		return SSOSettings{}, false, err
	}
	clientID, _, err := s.GetSetting(ctx, ssoClientIDKey)
	if err != nil {
		return SSOSettings{}, false, err
	}
	enc, _, err := s.GetSetting(ctx, ssoSecretKey)
	if err != nil {
		return SSOSettings{}, false, err
	}
	var secret string
	if enc != "" {
		if s.box == nil {
			return SSOSettings{}, false, fmt.Errorf("store: no secret box configured; cannot read the SSO client secret")
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return SSOSettings{}, false, fmt.Errorf("decrypt SSO client secret: %w", err)
		}
		secret = string(plain)
	}
	return SSOSettings{Issuer: issuer, ClientID: clientID, ClientSecret: secret}, true, nil
}

// SetSSOSettings stores the provider settings, sealing the client secret. The
// three rows are written together or not at all. endSessions also signs out
// every SSO account in the same transaction, so a failure can't save the new
// settings and leave the old sessions alive.
func (s *Store) SetSSOSettings(ctx context.Context, v SSOSettings, endSessions bool) error {
	if s.box == nil {
		return fmt.Errorf("store: no secret box configured; cannot store the SSO client secret")
	}
	enc, err := s.box.Seal([]byte(v.ClientSecret))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, val := range map[string]string{ssoIssuerKey: v.Issuer, ssoClientIDKey: v.ClientID, ssoSecretKey: enc} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, val); err != nil {
			return err
		}
	}
	if endSessions {
		if err := endSSOSessions(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearSSOSettings removes the provider settings and, in the same
// transaction, signs out every SSO account.
func (s *Store) ClearSSOSettings(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key IN (?, ?, ?)`,
		ssoIssuerKey, ssoClientIDKey, ssoSecretKey); err != nil {
		return err
	}
	if err := endSSOSessions(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// EndSSOSessions signs out every account that signs in with SSO. Password
// sessions are untouched.
func (s *Store) EndSSOSessions(ctx context.Context) error {
	return endSSOSessions(ctx, s.db)
}

// endSSOSessions ends the sessions of SSO accounts and every SSO-kind gateway
// session (route visits included, which have no account) with its route
// sessions. Password-kind gateway sessions stay.
func endSSOSessions(ctx context.Context, q execer) error {
	if _, err := q.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE sign_in = 'sso')`); err != nil {
		return err
	}
	return deleteGateSessionsOfKind(ctx, q, core.SignInSSO)
}
