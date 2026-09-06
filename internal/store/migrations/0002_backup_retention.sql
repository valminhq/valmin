-- Backup retention, per instance. Cold and hot archives are counted separately: a cold one
-- is quiesced and restorable, a hot one is a best-effort copy of a world that was mid-flight
-- (B12). Under one shared count a burst of cheap hot copies evicts every quiesced archive and
-- leaves a full catalogue with nothing worth restoring.
--
-- 0 keeps everything in that class. There is deliberately no spelling of "delete them all".
ALTER TABLE instances ADD COLUMN backup_keep_cold INTEGER NOT NULL DEFAULT 2;
ALTER TABLE instances ADD COLUMN backup_keep_hot INTEGER NOT NULL DEFAULT 5;

-- Take a cold archive between a restart's stop and its start. Off by default: the world is
-- already flushed and the container already down at that point, so the archive is nearly
-- free, but it still adds its own duration to a restart.
ALTER TABLE instances ADD COLUMN backup_on_restart BOOLEAN NOT NULL DEFAULT FALSE;
