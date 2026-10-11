ALTER TABLE instances ADD COLUMN remote_backup_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE instances ADD COLUMN remote_keep_cold INTEGER NOT NULL DEFAULT 10 CHECK (remote_keep_cold >= 0);
ALTER TABLE instances ADD COLUMN remote_keep_hot INTEGER NOT NULL DEFAULT 5 CHECK (remote_keep_hot >= 0);
ALTER TABLE instances ADD COLUMN remote_keep_snapshots INTEGER NOT NULL DEFAULT 10 CHECK (remote_keep_snapshots >= 0);

CREATE TABLE remote_backup_destinations (
 id TEXT PRIMARY KEY,
 kind TEXT NOT NULL CHECK (kind IN ('webdav', 'rclone')),
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 retired BOOLEAN NOT NULL DEFAULT FALSE,
 endpoint TEXT NOT NULL DEFAULT '',
 username TEXT NOT NULL DEFAULT '',
 credentials TEXT NOT NULL DEFAULT '',
 remote_name TEXT NOT NULL DEFAULT '',
 folder TEXT NOT NULL DEFAULT '',
 last_test_at TEXT,
 last_test_error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_remote_destination_active ON remote_backup_destinations ((1)) WHERE retired = FALSE;

-- Archive metadata outlives the local catalogue and deleted instances.
CREATE TABLE remote_copies (
 id TEXT PRIMARY KEY,
 destination_id TEXT NOT NULL REFERENCES remote_backup_destinations (id),
 instance_id TEXT NOT NULL,
 backup_id TEXT NOT NULL,
 instance_name TEXT NOT NULL,
 world_name TEXT NOT NULL,
 trigger TEXT NOT NULL,
 consistent BOOLEAN NOT NULL,
 size_bytes INTEGER NOT NULL,
 sha256 TEXT NOT NULL,
 source_path TEXT NOT NULL,
 archive_created_at TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending', 'uploading', 'retry_wait', 'succeeded', 'failed', 'cancelled', 'pruned')),
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TEXT NOT NULL,
 deadline_at TEXT NOT NULL,
 last_error TEXT NOT NULL DEFAULT '',
 cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
 job_id TEXT REFERENCES job_runs(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
 object_json TEXT NOT NULL DEFAULT '{}',
 manifest_json TEXT NOT NULL DEFAULT '{}',
 succeeded_at TEXT,
 cleanup_pending BOOLEAN NOT NULL DEFAULT FALSE,
 cleanup_error TEXT NOT NULL DEFAULT '',
 cleanup_next_at TEXT NOT NULL,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE (destination_id, backup_id)
);
CREATE INDEX idx_remote_copies_due ON remote_copies (status, next_attempt_at);
CREATE INDEX idx_remote_copies_source ON remote_copies (instance_id, backup_id, status);
CREATE INDEX idx_remote_copies_history ON remote_copies (instance_id, created_at DESC, id DESC);
