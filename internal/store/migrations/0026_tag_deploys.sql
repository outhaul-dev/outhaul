-- Tag deploys: an app can deploy on new version tags instead of every push to
-- its branch. Existing apps stay in branch mode. A deployment records the tag
-- it builds, and whether a tag push queued it (the worker re-checks those).
ALTER TABLE apps ADD COLUMN deploy_trigger  TEXT    NOT NULL DEFAULT 'branch';
ALTER TABLE apps ADD COLUMN tag_pattern     TEXT    NOT NULL DEFAULT 'v*';
ALTER TABLE apps ADD COLUMN tag_prereleases INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN tag            TEXT    NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN from_tag_event INTEGER NOT NULL DEFAULT 0;
