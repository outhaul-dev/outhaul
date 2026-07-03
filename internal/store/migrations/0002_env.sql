-- Per-app environment variables. `value` holds secretbox ciphertext (base64),
-- never plaintext. ON DELETE CASCADE ties env rows to their app.
CREATE TABLE app_env (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    is_secret  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    UNIQUE(app_id, key)
);
