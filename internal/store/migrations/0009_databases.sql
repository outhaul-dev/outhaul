-- Managed database containers (Postgres/MySQL/Redis). password is secretbox
-- ciphertext (same scheme as env values). No DB-level FK on project_id, to
-- match apps.project_id (migration 0004); the store layer guards integrity.
CREATE TABLE databases (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    INTEGER NOT NULL DEFAULT 1,
    name          TEXT NOT NULL UNIQUE,
    engine        TEXT NOT NULL,
    image         TEXT NOT NULL,
    username      TEXT NOT NULL DEFAULT '',
    password      TEXT NOT NULL,
    db_name       TEXT NOT NULL DEFAULT '',
    ext_port      INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'creating',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

CREATE INDEX idx_databases_project ON databases(project_id);
