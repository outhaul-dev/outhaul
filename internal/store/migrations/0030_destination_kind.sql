-- Backup destinations gain a kind: s3 (the existing form) or azure, whose one
-- row the environment owns and whose secret is empty.
ALTER TABLE destinations ADD COLUMN kind TEXT NOT NULL DEFAULT 's3';
