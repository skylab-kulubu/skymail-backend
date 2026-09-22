DROP INDEX IF EXISTS idx_mail_queue_active_jobs;

ALTER TABLE mail_queue
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS attempts;

CREATE INDEX idx_mail_queue_active_jobs
    ON mail_queue (status, created_at)
    WHERE status IN ('pending', 'processing');
