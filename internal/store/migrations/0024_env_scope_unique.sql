-- 0020 added app_env.scope but left UNIQUE(app_id, key), so writing a key in
-- one scope overwrote (and re-scoped) the row of another: adding a
-- preview-only DATABASE_URL silently deleted the production one. Rebuild the
-- table so each (app, key, scope) is its own row. Ids are kept.
CREATE TABLE app_env_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    is_secret  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    scope      TEXT    NOT NULL DEFAULT 'shared',
    UNIQUE(app_id, key, scope)
);
INSERT INTO app_env_new (id, app_id, key, value, is_secret, created_at, scope)
    SELECT id, app_id, key, value, is_secret, created_at, scope FROM app_env;
DROP TABLE app_env;
ALTER TABLE app_env_new RENAME TO app_env;
