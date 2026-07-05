-- Template apps: compose stacks created from the built-in catalog instead of
-- a Git repo. template_id records which catalog entry the app came from and
-- compose_raw holds the compose file snapshot the pipeline deploys (both
-- empty for repo-backed apps).
ALTER TABLE apps ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN compose_raw TEXT NOT NULL DEFAULT '';
