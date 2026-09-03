-- The single-App record is now carried by git_sources (0022) and nothing reads
-- it. Dropping it here rather than in 0022 kept every commit on the way to
-- multi-account support compiling and green.
DROP TABLE IF EXISTS github_app;
