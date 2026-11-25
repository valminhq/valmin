-- The config files a saved setup holds, by path under BepInEx/config. Their bytes live in the
-- setup blob store under sha256, shared by every setup holding the same bytes.
CREATE TABLE saved_setup_configs (
    setup_id   TEXT NOT NULL REFERENCES saved_setups (id) ON DELETE CASCADE,
    file       TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    PRIMARY KEY (setup_id, file)
);

CREATE INDEX idx_saved_setup_configs_sha256 ON saved_setup_configs (sha256);

-- A setup the panel saved on its own before a restore, kept only as long as retention allows.
ALTER TABLE saved_setups ADD COLUMN automatic BOOLEAN NOT NULL DEFAULT FALSE;

-- Whether the config tree held files past the listing limits, which the setup left out.
ALTER TABLE saved_setups ADD COLUMN configs_truncated BOOLEAN NOT NULL DEFAULT FALSE;
