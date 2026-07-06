-- Link an app to a managed database, injected as env_var at deploy time.
-- The connection string is NOT stored: it is computed from the databases row
-- on every deploy (dbaas.InternalURL), so credential rotation propagates.
-- app_id cascades on app delete; database_id has no cascade so a delete that
-- would orphan a live app is refused by DeleteDatabase's guard instead.
CREATE TABLE attachments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    database_id INTEGER NOT NULL REFERENCES databases(id),
    env_var     TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    UNIQUE(app_id, env_var)
);
