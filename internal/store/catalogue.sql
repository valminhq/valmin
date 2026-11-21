-- The mod catalogue: a cache of each registry's listing, kept apart from the panel's own
-- database and rebuilt by the next sync whenever this file is new. It has no migrations: a
-- build with another schema version deletes the file and starts over.

-- One package as one registry lists it. unlisted_at is when a complete listing of the registry
-- last omitted it, null while the registry carries it.
CREATE TABLE mod_packages (
    full_name      TEXT NOT NULL,
    source         TEXT NOT NULL,
    namespace      TEXT NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT,
    latest_version TEXT,
    downloads      INTEGER,
    rating         INTEGER,
    is_deprecated  BOOLEAN NOT NULL DEFAULT FALSE,
    categories     TEXT,
    icon_url       TEXT,
    unlisted_at    TIMESTAMP,
    PRIMARY KEY (full_name, source)
);

CREATE TABLE mod_versions (
    full_name    TEXT NOT NULL,
    source       TEXT NOT NULL,
    version      TEXT NOT NULL,
    dependencies TEXT NOT NULL,
    download_url TEXT NOT NULL,
    file_size    INTEGER,
    PRIMARY KEY (full_name, source, version)
);

-- Each registry's sync state: its ETag, when it last synced, the last result, and when a
-- listing last completed. It lives with the rows it describes, so a new file starts with none.
CREATE TABLE catalogue_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
