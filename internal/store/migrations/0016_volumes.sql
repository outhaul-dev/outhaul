-- Persistent volumes for single-container apps. Compose stacks declare their
-- own volumes in the compose file and are not tracked here.
CREATE TABLE volumes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    mount_path TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    UNIQUE(app_id, mount_path),
    UNIQUE(name)
);
