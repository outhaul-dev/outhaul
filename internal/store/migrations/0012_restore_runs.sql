-- Restores share the backup run history: kind tells the panel whether a row
-- shipped an archive out (backup) or brought one back (restore).
ALTER TABLE backup_runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'backup';
