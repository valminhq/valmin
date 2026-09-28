-- A version lock on an installed package. A locked package keeps its version: Update all skips
-- it and dependency resolution reports a conflict rather than moving it.
ALTER TABLE instance_mods ADD COLUMN locked BOOLEAN NOT NULL DEFAULT FALSE;
