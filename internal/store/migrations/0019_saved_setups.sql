CREATE UNIQUE INDEX idx_backups_instance_id ON backups (instance_id, id);

CREATE TABLE saved_setups (
    id            TEXT PRIMARY KEY,
    instance_id   TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    game_build_id TEXT NOT NULL,
    world_name    TEXT NOT NULL,
    snapshot_json TEXT NOT NULL,
    backup_id     TEXT,
    created_at    TIMESTAMP NOT NULL,
    FOREIGN KEY (instance_id, backup_id) REFERENCES backups (instance_id, id)
        ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idx_saved_setups_instance ON saved_setups (instance_id, created_at DESC, id DESC);
CREATE INDEX idx_saved_setups_backup ON saved_setups (backup_id);

CREATE TABLE saved_setup_artifacts (
    setup_id  TEXT NOT NULL REFERENCES saved_setups (id) ON DELETE CASCADE,
    full_name TEXT NOT NULL,
    source    TEXT NOT NULL,
    version   TEXT NOT NULL,
    kind      TEXT NOT NULL CHECK (kind IN ('zip', 'files')),
    sha256    TEXT NOT NULL,
    PRIMARY KEY (setup_id, full_name)
);

CREATE INDEX idx_saved_setup_artifacts_sha256 ON saved_setup_artifacts (sha256);
