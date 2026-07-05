-- Dockerfile-built apps: where the Dockerfile lives, relative to the repo
-- root. Empty for other kinds; normalized to 'Dockerfile' by the UI layer.
ALTER TABLE apps ADD COLUMN dockerfile_path TEXT NOT NULL DEFAULT '';
