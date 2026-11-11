package store

import (
	"context"
	"fmt"
)

// sealedColumn is one column whose non-empty values are sealed with the box.
// A new sealed column must be added here, or a lost secret.key could be
// silently replaced over it (see HasSealedValues).
type sealedColumn struct {
	table  string
	column string
	where  string // "" = every row; otherwise a SQL condition on that table
}

var sealedColumns = []sealedColumn{
	{"apps", "ssh_private_key", "ssh_private_key <> ''"},
	{"app_env", "value", ""},
	{"project_env", "value", ""},
	{"databases", "password", "password <> ''"},
	{"destinations", "secret_key", "secret_key <> ''"},
	{"github_app_sources", "private_key", ""},
	{"github_app_sources", "webhook_secret", ""},
	{"github_app_sources", "client_secret", ""},
	{"settings", "value", "key = '" + cloudflareTokenKey + "'"},
	{"settings", "value", "key = '" + ssoSecretKey + "'"},
}

func (c sealedColumn) selectSQL() string {
	q := "SELECT " + c.column + " FROM " + c.table
	if c.where != "" {
		q += " WHERE " + c.where
	}
	return q
}

// HasSealedValues reports whether any stored value is sealed with the secret
// box. When the key file is missing, a fresh key may be generated only if
// this is false: values sealed with the old key would be unreadable.
func (s *Store) HasSealedValues(ctx context.Context) (bool, error) {
	for _, c := range sealedColumns {
		rows, err := s.db.QueryContext(ctx, c.selectSQL()+" LIMIT 1")
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

// CheckSealed opens every sealed value with s.box and returns how many it
// opened. It is how restore-state proves a supplied key fits the copy.
func (s *Store) CheckSealed(ctx context.Context) (int, error) {
	n := 0
	for _, c := range sealedColumns {
		rows, err := s.db.QueryContext(ctx, c.selectSQL())
		if err != nil {
			return n, fmt.Errorf("read %s.%s: %w", c.table, c.column, err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return n, fmt.Errorf("read %s.%s: %w", c.table, c.column, err)
			}
			if v == "" {
				continue
			}
			if s.box == nil {
				rows.Close()
				return n, fmt.Errorf("%s.%s holds a sealed value; --key-file is required", c.table, c.column)
			}
			if _, err := s.box.Open(v); err != nil {
				rows.Close()
				return n, fmt.Errorf("decryption failed for %s.%s: %w", c.table, c.column, err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return n, fmt.Errorf("read %s.%s: %w", c.table, c.column, err)
		}
		rows.Close()
	}
	return n, nil
}
