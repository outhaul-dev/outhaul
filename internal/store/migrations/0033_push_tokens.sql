-- Push tokens: credentials for git over HTTPS. Only the SHA-256 of the value
-- is stored. last_used_at is NULL until first use, as push_keys does.
CREATE TABLE push_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label        TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_used_at TEXT,
    last_used_ip TEXT    NOT NULL DEFAULT '',
    UNIQUE (user_id, label)
);
CREATE INDEX push_tokens_user ON push_tokens(user_id);
