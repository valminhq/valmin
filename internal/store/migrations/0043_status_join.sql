-- Whether the public status page shows how to join and the server's current profile code.
ALTER TABLE instances ADD COLUMN status_show_join BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE instances ADD COLUMN status_show_modpack BOOLEAN NOT NULL DEFAULT FALSE;

-- The last profile code uploaded for each server's player modpack, with a digest of the mod
-- list it carries, so the code is offered only while the server's mods still match it.
CREATE TABLE modpack_codes (
    instance_id     TEXT PRIMARY KEY REFERENCES instances (id) ON DELETE CASCADE,
    code            TEXT NOT NULL,
    registry        TEXT NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL
);
