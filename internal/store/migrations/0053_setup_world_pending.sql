-- A setup waiting to be linked to the world backup the next mod change takes. Any change to the
-- server's installed mods ends the wait. A migration that rebuilds instance_mods must create
-- these triggers again.
ALTER TABLE saved_setups ADD COLUMN world_pending BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TRIGGER instance_mods_insert_ends_world_wait AFTER INSERT ON instance_mods
BEGIN
    UPDATE saved_setups SET world_pending = FALSE
    WHERE instance_id = NEW.instance_id AND world_pending;
END;

CREATE TRIGGER instance_mods_delete_ends_world_wait AFTER DELETE ON instance_mods
BEGIN
    UPDATE saved_setups SET world_pending = FALSE
    WHERE instance_id = OLD.instance_id AND world_pending;
END;

CREATE TRIGGER instance_mods_update_ends_world_wait
AFTER UPDATE OF source, version, enabled ON instance_mods
WHEN OLD.source IS NOT NEW.source OR OLD.version IS NOT NEW.version OR OLD.enabled IS NOT NEW.enabled
BEGIN
    UPDATE saved_setups SET world_pending = FALSE
    WHERE instance_id = NEW.instance_id AND world_pending;
END;
