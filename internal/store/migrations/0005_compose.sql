-- Compose apps: kind discriminates the deploy strategy (existing rows stay
-- nixpacks); compose_* locate the stack's file and its (optional) exposed
-- service. watch_paths applies to BOTH kinds: newline-separated glob patterns
-- gating auto-deploy on which files a push changed (empty = every push).
ALTER TABLE apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'nixpacks';
ALTER TABLE apps ADD COLUMN compose_path TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN compose_service TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN compose_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN watch_paths TEXT NOT NULL DEFAULT '';
