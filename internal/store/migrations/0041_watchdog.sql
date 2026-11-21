-- What to do about a running server whose log has gone quiet: off, alert, or restart.
ALTER TABLE instances ADD COLUMN watchdog TEXT NOT NULL DEFAULT 'off';
