-- Two registries feed one index (03 §6.1), so a package's identity is (full_name, source):
-- the same full_name exists on both, each with its own versions, download URLs and bytes.
-- Every row that exists when this runs was written by the only client that existed, so it
-- backfills to 'thunderstore'.
--
-- The set of registries is closed in Go by a typed constant, so there is no CHECK here: a
-- CHECK would make adding a registry another table rebuild for no additional safety.
CREATE TABLE mod_packages_new (
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
    synced_at      TIMESTAMP NOT NULL,
    PRIMARY KEY (full_name, source)
);

INSERT INTO mod_packages_new (
    full_name, source, namespace, name, description, latest_version,
    downloads, rating, is_deprecated, categories, icon_url, synced_at
)
SELECT full_name, 'thunderstore', namespace, name, description, latest_version,
    downloads, rating, is_deprecated, categories, icon_url, synced_at
FROM mod_packages;

DROP TABLE mod_packages;

ALTER TABLE mod_packages_new RENAME TO mod_packages;

CREATE TABLE mod_versions_new (
    full_name    TEXT NOT NULL,
    source       TEXT NOT NULL,
    version      TEXT NOT NULL,
    dependencies TEXT NOT NULL,
    download_url TEXT NOT NULL,
    file_size    INTEGER,
    PRIMARY KEY (full_name, source, version)
);

INSERT INTO mod_versions_new (
    full_name, source, version, dependencies, download_url, file_size
)
SELECT full_name, 'thunderstore', version, dependencies, download_url, file_size
FROM mod_versions;

DROP TABLE mod_versions;

ALTER TABLE mod_versions_new RENAME TO mod_versions;

-- The registry an installed package's files came from. The primary key stays
-- (instance_id, full_name): both registries place a package at the same path under the
-- server root (03 §6.4), so an instance holds it once, from one registry, and the file
-- manifest that makes uninstall exact (ADR-009) describes one registry's bytes.
ALTER TABLE instance_mods ADD COLUMN source TEXT NOT NULL DEFAULT 'thunderstore';
