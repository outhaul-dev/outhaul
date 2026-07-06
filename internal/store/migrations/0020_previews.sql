-- Preview environments: ephemeral child apps per GitHub PR.
ALTER TABLE apps ADD COLUMN parent_id      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN pr_number      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN ephemeral      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN preview_status TEXT    NOT NULL DEFAULT '';

-- Per-app preview configuration; a row exists only once previews are enabled.
CREATE TABLE preview_configs (
    app_id         INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    enabled        INTEGER NOT NULL DEFAULT 0,
    base_domain    TEXT    NOT NULL DEFAULT '',
    post_pr_comment INTEGER NOT NULL DEFAULT 1,
    allow_fork_prs INTEGER NOT NULL DEFAULT 0,
    idle_ttl_days  INTEGER NOT NULL DEFAULT 7,
    max_concurrent INTEGER NOT NULL DEFAULT 5
);

-- Env var scope: '', 'shared', 'prod', or 'preview' ('' == shared).
ALTER TABLE app_env ADD COLUMN scope TEXT NOT NULL DEFAULT 'shared';
