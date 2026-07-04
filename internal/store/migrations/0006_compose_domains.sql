-- Compose domains: a stack may publish any number of services on any number
-- of hosts (Dokploy's model), so the single domain/compose_service/
-- compose_port trio moves off apps into its own table. Existing exposure is
-- backfilled as the stack's first domain row; apps.domain then reverts to
-- being the nixpacks-only column.
CREATE TABLE compose_domains (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    domain     TEXT    NOT NULL,
    service    TEXT    NOT NULL,
    port       INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    UNIQUE(app_id, domain)
);

INSERT INTO compose_domains (app_id, domain, service, port, created_at)
SELECT id, domain, compose_service, compose_port, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  FROM apps
 WHERE kind = 'compose' AND domain != '' AND compose_service != '';

UPDATE apps SET domain = '' WHERE kind = 'compose';
ALTER TABLE apps DROP COLUMN compose_service;
ALTER TABLE apps DROP COLUMN compose_port;
