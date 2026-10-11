package store

import "context"

// sealedQueries each find at least one value sealed with the secret box.
// A new sealed column must be added here, or a lost secret.key could be
// silently replaced over it (see HasSealedValues).
var sealedQueries = []string{
	`SELECT 1 FROM apps WHERE ssh_private_key <> '' LIMIT 1`,
	`SELECT 1 FROM app_env LIMIT 1`,
	`SELECT 1 FROM project_env LIMIT 1`,
	`SELECT 1 FROM databases LIMIT 1`,
	`SELECT 1 FROM destinations LIMIT 1`,
	`SELECT 1 FROM github_app_sources LIMIT 1`,
	`SELECT 1 FROM settings WHERE key = '` + cloudflareTokenKey + `' LIMIT 1`,
}

// HasSealedValues reports whether any stored value is sealed with the secret
// box. When the key file is missing, a fresh key may be generated only if
// this is false: values sealed with the old key would be unreadable.
func (s *Store) HasSealedValues(ctx context.Context) (bool, error) {
	for _, q := range sealedQueries {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return false, err
		}
		found := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}
