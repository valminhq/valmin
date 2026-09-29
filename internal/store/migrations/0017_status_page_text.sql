-- Administrator text for the public status page: a notice such as a maintenance announcement,
-- and guidance on how to join. Both are plain text, empty when unset, and served only while
-- status_published is true.
ALTER TABLE instances ADD COLUMN status_notice TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN status_connect_info TEXT NOT NULL DEFAULT '';
