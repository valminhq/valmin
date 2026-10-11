-- A config or mod change waiting for the next start. Separate from restart_required, which
-- remains the flag for launch settings, a new password and a restored setup.
ALTER TABLE instances ADD COLUMN pending_restart BOOLEAN NOT NULL DEFAULT FALSE;
