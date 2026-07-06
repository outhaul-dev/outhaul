-- Global account SSH public keys authorized to push (git-push-to-deploy).
-- Any registered key may push to any push-source app.
CREATE TABLE push_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    label        TEXT    NOT NULL,
    fingerprint  TEXT    NOT NULL UNIQUE,
    public_key   TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT
);
