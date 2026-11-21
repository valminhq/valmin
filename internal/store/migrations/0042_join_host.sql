-- The host name or IP address players connect to, without a port. Empty when unset.
ALTER TABLE instances ADD COLUMN join_host TEXT NOT NULL DEFAULT '';
