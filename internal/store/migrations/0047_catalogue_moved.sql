-- The mod catalogue moved to its own file under data.root/cache, attached to every panel
-- connection as "catalogue". Its rows and sync state are copied across first, replacing
-- whatever the file held, so the panel keeps its catalogue through the upgrade. A package the
-- last complete listing of its registry left out carries that as unlisted_at; timestamps are
-- fixed-width text, so they compare as strings.
DELETE FROM catalogue.mod_versions;
DELETE FROM catalogue.mod_packages;
DELETE FROM catalogue.catalogue_state;

INSERT INTO catalogue.mod_packages (
    full_name, source, namespace, name, description, latest_version,
    downloads, rating, is_deprecated, categories, icon_url, unlisted_at
)
SELECT p.full_name, p.source, p.namespace, p.name, p.description, p.latest_version,
       p.downloads, p.rating, p.is_deprecated, p.categories, p.icon_url,
       CASE WHEN p.synced_at < (
           SELECT SUBSTR(k.value, 2, LENGTH(k.value) - 2) FROM main.kv k
           WHERE k.key = p.source || '_listing_started_at'
       ) THEN p.synced_at END
FROM main.mod_packages p;

INSERT INTO catalogue.mod_versions (full_name, source, version, dependencies, download_url, file_size)
SELECT full_name, source, version, dependencies, download_url, file_size FROM main.mod_versions;

-- The listing start becomes the completion the catalogue records: both say a whole listing
-- landed. Values are already JSON, as the catalogue state stores them.
INSERT INTO catalogue.catalogue_state (key, value, updated_at)
SELECT REPLACE(key, '_listing_started_at', '_listing_completed_at'), value, updated_at
FROM main.kv
WHERE key IN (
    'thunderstore_etag', 'thunderstore_synced_at', 'thunderstore_sync_result',
    'thunderstore_listing_started_at',
    'hexium_etag', 'hexium_synced_at', 'hexium_sync_result', 'hexium_listing_started_at'
);

DROP TABLE main.mod_versions;
DROP TABLE main.mod_packages;
DELETE FROM main.kv WHERE key IN (
    'thunderstore_etag', 'thunderstore_synced_at', 'thunderstore_sync_result',
    'thunderstore_listing_started_at',
    'hexium_etag', 'hexium_synced_at', 'hexium_sync_result', 'hexium_listing_started_at'
);
