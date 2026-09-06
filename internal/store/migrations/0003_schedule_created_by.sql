-- Who set a schedule up. Audit only: the clock never reads it, so a schedule keeps firing
-- after its author's grant is revoked or their account is deleted (ADR-134). Backups that
-- silently stop running because somebody left the group is the failure this milestone exists
-- to prevent, and "the author can still do this today" is not a question a 03:00 tick should
-- be asking.
--
-- ON DELETE SET NULL rather than CASCADE: deleting a user must not delete the panel's
-- schedules. NULL therefore means either a run the scheduler itself created or an author who
-- is gone, and the API renders both as an author it cannot name.
ALTER TABLE scheduled_jobs ADD COLUMN created_by TEXT REFERENCES users (id) ON DELETE SET NULL;
