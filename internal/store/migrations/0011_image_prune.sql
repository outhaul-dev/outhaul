-- Image retention: image_pruned marks deployments whose built image has been
-- removed from the host by the pruner. The image column keeps its value (the
-- history stays truthful); the flag is what hides the Rollback action and
-- keeps the tag out of the retained set.
ALTER TABLE deployments ADD COLUMN image_pruned INTEGER NOT NULL DEFAULT 0;
