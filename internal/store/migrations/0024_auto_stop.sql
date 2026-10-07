-- Stop a running server once it has had no players for this many minutes. 0 turns it off.
ALTER TABLE instances ADD COLUMN auto_stop_minutes INTEGER NOT NULL DEFAULT 0;
