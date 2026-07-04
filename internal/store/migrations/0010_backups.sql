-- Scheduled backups to S3-compatible storage: destinations (bucket configs,
-- secret_key is secretbox ciphertext), backups (a schedule attaching a target
-- to a destination), backup_runs (execution history). target_id references
-- databases.id or apps.id depending on target_kind; no DB-level FK for the
-- same reason as apps.project_id (store layer guards + cascades).
CREATE TABLE destinations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    endpoint    TEXT NOT NULL,
    region      TEXT NOT NULL DEFAULT '',
    bucket      TEXT NOT NULL,
    access_key  TEXT NOT NULL,
    secret_key  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE backups (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    target_kind     TEXT NOT NULL,
    target_id       INTEGER NOT NULL,
    destination_id  INTEGER NOT NULL REFERENCES destinations(id),
    schedule        TEXT NOT NULL,
    prefix          TEXT NOT NULL DEFAULT '',
    retention       INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL
);

CREATE INDEX idx_backups_target ON backups(target_kind, target_id);

CREATE TABLE backup_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    backup_id   INTEGER NOT NULL REFERENCES backups(id) ON DELETE CASCADE,
    status      TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    object_key  TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    finished_at TEXT
);

CREATE INDEX idx_backup_runs_backup ON backup_runs(backup_id, started_at);
