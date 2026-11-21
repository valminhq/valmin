-- The daemon holds a lock file in data.root instead of a heartbeat row.
DELETE FROM kv WHERE key = 'daemon_lease';
