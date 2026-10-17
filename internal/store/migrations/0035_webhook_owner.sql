-- instance_id is the server a webhook belongs to; NULL shares it with every server and with
-- panel-wide rules. catch_all is whether the alerts no rule routes reach it: an unexpected stop,
-- a failed backup and a new game build.
ALTER TABLE webhooks ADD COLUMN instance_id TEXT REFERENCES instances (id) ON DELETE CASCADE;
ALTER TABLE webhooks ADD COLUMN catch_all BOOLEAN NOT NULL DEFAULT TRUE;

-- A name is unique among the shared webhooks and among each server's own.
DROP INDEX webhooks_name;
CREATE UNIQUE INDEX webhooks_name ON webhooks (COALESCE(instance_id, ''), name);
