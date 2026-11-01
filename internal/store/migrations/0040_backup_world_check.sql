-- What the world check found in each archive. world_bytes is everything the world owns in it:
-- the .db/.fwl pair, or the whole 1.0 world directory. world_attention is a JSON array of
-- {code, message} findings the operator should look at, NULL when there are none. Both stay NULL
-- for archives taken before the check existed and for snapshots, which are kept unverified.
ALTER TABLE backups ADD COLUMN world_bytes INTEGER;
ALTER TABLE backups ADD COLUMN world_attention TEXT;

-- Whether the archive a remote copy was made from was flagged, copied when the upload is queued
-- and cleared when the archive is acknowledged. Remote retention keeps the newest unflagged copy
-- of each class, and the local archive it came from may be gone by then.
ALTER TABLE remote_copies ADD COLUMN world_flagged BOOLEAN NOT NULL DEFAULT FALSE;
