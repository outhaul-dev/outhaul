-- Project-level shared environment variables (Dokploy's model): a per-project
-- dictionary that apps opt into by referencing ${{project.KEY}} inside their
-- own env values, resolved at deploy time. Never injected unreferenced.
-- `value` holds secretbox ciphertext (base64), never plaintext.
CREATE TABLE project_env (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    is_secret  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    UNIQUE(project_id, key)
);
