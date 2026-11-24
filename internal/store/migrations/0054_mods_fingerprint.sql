-- The managed mods an archive's world was played with, and the mods a setup holds, as one hash
-- each. A backup fits a setup when the two are equal. NULL where the mods were never recorded.
ALTER TABLE backups ADD COLUMN mods_fingerprint TEXT;
ALTER TABLE saved_setups ADD COLUMN mods_fingerprint TEXT;
CREATE INDEX idx_backups_mods_fingerprint ON backups (instance_id, mods_fingerprint);
-- The mods the world on disk was last played with: those installed when the server last started,
-- or those of the backup it was restored from. NULL until the panel first records them.
ALTER TABLE instances ADD COLUMN world_mods_fingerprint TEXT;
