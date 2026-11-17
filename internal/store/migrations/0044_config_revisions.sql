-- Earlier versions of each server's config files. A row holds the file's bytes at one moment
-- and what produced them; the baseline row is the version a template code measures tweaks
-- against.
CREATE TABLE config_revisions (
    id             TEXT PRIMARY KEY,
    instance_id    TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    file           TEXT NOT NULL,
    created_at     TIMESTAMP NOT NULL,
    source         TEXT NOT NULL,
    user_id        TEXT REFERENCES users (id) ON DELETE SET NULL,
    mod            TEXT NOT NULL DEFAULT '',
    from_version   TEXT NOT NULL DEFAULT '',
    to_version     TEXT NOT NULL DEFAULT '',
    replaced_edits BOOLEAN NOT NULL DEFAULT FALSE,
    baseline       BOOLEAN NOT NULL DEFAULT FALSE,
    sha256         TEXT NOT NULL,
    content        BLOB NOT NULL
);

CREATE INDEX config_revisions_file ON config_revisions (instance_id, file, created_at);
