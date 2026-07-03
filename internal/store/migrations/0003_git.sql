-- M3 git automation: per-app source/auth + branch + auto-deploy, and a
-- single-row GitHub App record. New app columns default so existing rows become
-- public/main with auto-deploy off. Existing rows get a random webhook secret.
ALTER TABLE apps ADD COLUMN branch          TEXT    NOT NULL DEFAULT 'main';
ALTER TABLE apps ADD COLUMN auto_deploy     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN source          TEXT    NOT NULL DEFAULT 'public';
ALTER TABLE apps ADD COLUMN webhook_secret  TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN ssh_private_key TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN ssh_public_key  TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN github_repo     TEXT    NOT NULL DEFAULT '';

UPDATE apps SET webhook_secret = lower(hex(randomblob(16))) WHERE webhook_secret = '';

CREATE TABLE github_app (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    app_id          INTEGER NOT NULL,
    slug            TEXT    NOT NULL,
    private_key     TEXT    NOT NULL,
    webhook_secret  TEXT    NOT NULL,
    client_id       TEXT    NOT NULL,
    client_secret   TEXT    NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL
);
