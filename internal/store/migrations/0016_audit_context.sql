-- Names as they were when the entry was written, so renaming or deleting a user or server does
-- not rewrite history. job_id links an entry to the job it requested; outcome records how a
-- direct action ended, while a job-backed entry reads its outcome from the job row.
ALTER TABLE audit_log ADD COLUMN actor_name TEXT;
ALTER TABLE audit_log ADD COLUMN instance_name TEXT;
ALTER TABLE audit_log ADD COLUMN job_id TEXT;
ALTER TABLE audit_log ADD COLUMN outcome TEXT;

CREATE INDEX idx_audit_log_action ON audit_log (action, created_at);
CREATE INDEX idx_audit_log_user ON audit_log (user_id, created_at);
CREATE INDEX idx_audit_log_job ON audit_log (job_id);
