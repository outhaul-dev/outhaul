-- Unify per-app routing into one `domains` table for every app kind, adding
-- path-based routing (external Path + InternalPath rewrite) and a per-domain
-- TLS toggle. compose_domains folds in unchanged; nixpacks/dockerfile apps with
-- a single domain each backfill one row on port 8080. apps.domain stays as an
-- auto-maintained "primary" mirror the list views read.
CREATE TABLE domains (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id        INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    host          TEXT    NOT NULL,
    service       TEXT    NOT NULL DEFAULT '',
    port          INTEGER NOT NULL,
    path          TEXT    NOT NULL DEFAULT '',
    internal_path TEXT    NOT NULL DEFAULT '',
    tls           INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT    NOT NULL,
    UNIQUE(app_id, host, path)
);

INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, created_at)
SELECT app_id, domain, service, port, '', '', 1, created_at FROM compose_domains;

DROP TABLE compose_domains;

-- Single-domain (nixpacks/dockerfile) apps become domain rows on port 8080.
INSERT INTO domains (app_id, host, service, port, path, internal_path, tls, created_at)
SELECT id, domain, '', 8080, '', '', 1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  FROM apps
 WHERE domain != '' AND kind IN ('nixpacks', 'dockerfile');
