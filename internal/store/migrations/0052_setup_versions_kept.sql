-- How many automatic setups a server keeps, and how many of those, newest first, keep their link
-- to a world backup.
ALTER TABLE instances ADD COLUMN setup_versions_kept INTEGER NOT NULL DEFAULT 10
    CHECK (setup_versions_kept BETWEEN 1 AND 100);
ALTER TABLE instances ADD COLUMN setup_worlds_kept INTEGER NOT NULL DEFAULT 3
    CHECK (setup_worlds_kept BETWEEN 0 AND 100);
