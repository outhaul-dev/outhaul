-- Projects: a grouping layer above apps (Dokploy-style workspaces). A Default
-- project is created and all existing apps are backfilled into it, so
-- project_id is always meaningful. No REFERENCES clause on project_id: SQLite
-- only allows ADD COLUMN foreign keys with DEFAULT NULL, and a NOT NULL FK
-- would force a full rebuild of apps. Referential integrity is enforced at the
-- store layer instead (project delete is refused while apps reference it).
CREATE TABLE projects (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

INSERT INTO projects (name, description, created_at)
VALUES ('default', '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

ALTER TABLE apps ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1;
