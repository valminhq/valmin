-- A schedule's player policy: whether a due run waits for the server to empty, for how long at
-- most, and what an unknown player count means. deferred_since is set while a due run is held.
ALTER TABLE scheduled_jobs ADD COLUMN wait_for_empty BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE scheduled_jobs ADD COLUMN max_deferral_seconds INTEGER NOT NULL DEFAULT 7200;
ALTER TABLE scheduled_jobs ADD COLUMN unknown_players TEXT NOT NULL DEFAULT 'wait'
    CHECK (unknown_players IN ('wait', 'run'));
ALTER TABLE scheduled_jobs ADD COLUMN deferred_since TIMESTAMP;
