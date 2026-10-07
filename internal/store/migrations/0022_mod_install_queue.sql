-- Mod installs requested while a server ran, applied once it is stopped, oldest first.
CREATE TABLE queued_mod_installs (
    instance_id  TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    full_name    TEXT NOT NULL,
    version      TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT '',
    requested_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (instance_id, full_name)
);

-- A restart that stopped a server to apply its queued installs, owed a start once they ran.
CREATE TABLE queued_mod_starts (
    instance_id  TEXT PRIMARY KEY REFERENCES instances (id) ON DELETE CASCADE,
    requested_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMP NOT NULL
);
