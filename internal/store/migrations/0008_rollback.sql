-- Rollbacks: a deployment can reuse a previous deployment's built image
-- instead of cloning and building. rollback_of records which deployment's
-- image was reused (0 = a normal build-from-source deploy); the image column
-- is pre-set at enqueue time for such rows.
ALTER TABLE deployments ADD COLUMN rollback_of INTEGER NOT NULL DEFAULT 0;
