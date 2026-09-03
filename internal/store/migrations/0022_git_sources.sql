-- Multiple connected Git accounts. git_sources is the generic identity record;
-- credentials live in a per-kind table so a future provider adds its own table
-- instead of widening a shared one with nullable columns. The legacy single-row
-- github_app is copied here (sealed values verbatim — migrations do no crypto)
-- and dropped in a later migration, once no code reads it.
CREATE TABLE git_sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    kind          TEXT NOT NULL,
    account_login TEXT NOT NULL DEFAULT '',
    account_type  TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

CREATE TABLE github_app_sources (
    source_id       INTEGER PRIMARY KEY REFERENCES git_sources(id) ON DELETE CASCADE,
    app_id          INTEGER NOT NULL,
    slug            TEXT    NOT NULL,
    private_key     TEXT    NOT NULL,
    webhook_secret  TEXT    NOT NULL,
    client_id       TEXT    NOT NULL,
    client_secret   TEXT    NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX github_app_sources_app_id ON github_app_sources(app_id);

INSERT INTO git_sources (kind, account_login, account_type, created_at)
    SELECT 'github_app', '', '', created_at FROM github_app WHERE id = 1;

INSERT INTO github_app_sources
    (source_id, app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id)
    SELECT (SELECT id FROM git_sources ORDER BY id LIMIT 1),
           app_id, slug, private_key, webhook_secret, client_id, client_secret, installation_id
    FROM github_app WHERE id = 1;

ALTER TABLE apps ADD COLUMN git_source_id INTEGER NOT NULL DEFAULT 0;

UPDATE apps SET git_source_id = (SELECT id FROM git_sources ORDER BY id LIMIT 1)
    WHERE source = 'github' AND EXISTS (SELECT 1 FROM git_sources);
