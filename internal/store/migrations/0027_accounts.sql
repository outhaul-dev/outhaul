ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'none';
ALTER TABLE users ADD COLUMN sign_in TEXT NOT NULL DEFAULT 'password';
ALTER TABLE users ADD COLUMN sso_issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN sso_subject TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN last_sign_in_at TEXT;

-- Every account that exists before this migration is an admin.
UPDATE users SET role = 'system_admin';

CREATE UNIQUE INDEX idx_users_sso ON users(sso_issuer, sso_subject) WHERE sign_in = 'sso';

CREATE TABLE project_members (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (project_id, user_id)
);

CREATE TABLE setup_links (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

ALTER TABLE push_keys ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE;
UPDATE push_keys SET user_id = (SELECT MIN(id) FROM users);
