package store

import "context"

// cloudflareTokenKey is the settings row holding the sealed Cloudflare Tunnel
// connector token. Its presence means the tunnel is enabled.
const cloudflareTokenKey = "cloudflare_tunnel_token"

// CloudflareToken returns the decrypted Cloudflare Tunnel connector token. ok is
// false (nil error) when no token is configured.
func (s *Store) CloudflareToken(ctx context.Context) (token string, ok bool, err error) {
	enc, ok, err := s.GetSetting(ctx, cloudflareTokenKey)
	if err != nil || !ok {
		return "", ok, err
	}
	plain, err := s.box.Open(enc)
	if err != nil {
		return "", false, err
	}
	return string(plain), true, nil
}

// SetCloudflareToken seals and stores the connector token (enabling the tunnel).
func (s *Store) SetCloudflareToken(ctx context.Context, token string) error {
	enc, err := s.box.Seal([]byte(token))
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, cloudflareTokenKey, enc)
}

// ClearCloudflareToken removes the stored token (disabling the tunnel).
func (s *Store) ClearCloudflareToken(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, cloudflareTokenKey)
	return err
}

// TunnelEnabled reports whether a Cloudflare Tunnel token is configured.
func (s *Store) TunnelEnabled(ctx context.Context) (bool, error) {
	_, ok, err := s.GetSetting(ctx, cloudflareTokenKey)
	return ok, err
}
