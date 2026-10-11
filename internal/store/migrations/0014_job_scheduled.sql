-- Whether a schedule started the job. Unlike schedule_id, which deleting the schedule sets to
-- NULL, it survives the delete, so a removed schedule's runs stay in the scheduled history.
ALTER TABLE job_runs ADD COLUMN scheduled BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE job_runs SET scheduled = TRUE WHERE schedule_id IS NOT NULL;
