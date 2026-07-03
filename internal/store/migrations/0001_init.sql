-- Initial schema. Timestamps are RFC3339Nano TEXT (human-debuggable in the DB
-- file); nullable time columns are left NULL until set.

CREATE TABLE apps (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    repo_url   TEXT NOT NULL,
    domain     TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE deployments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id      INTEGER NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    image       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT
);

CREATE INDEX idx_deployments_app ON deployments(app_id, created_at);
CREATE INDEX idx_deployments_status ON deployments(status);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    token      TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
