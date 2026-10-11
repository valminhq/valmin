-- Existing schedules keep their UTC meaning. New schedules may use an IANA location.
ALTER TABLE scheduled_jobs ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
