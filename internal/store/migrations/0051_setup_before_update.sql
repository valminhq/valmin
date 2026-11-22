-- What a mod update saves of the server's setup before it changes anything: a full setup, a
-- light one, or nothing beyond the world backup.
ALTER TABLE instances ADD COLUMN setup_before_update TEXT NOT NULL DEFAULT 'full'
    CHECK (setup_before_update IN ('full', 'light', 'off'));
