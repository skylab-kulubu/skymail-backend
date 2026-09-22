ALTER TABLE mail_queue
    ADD COLUMN attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

DROP INDEX IF EXISTS idx_mail_queue_active_jobs;

CREATE INDEX idx_mail_queue_active_jobs
    ON mail_queue (status, next_attempt_at, created_at)
    WHERE status IN ('pending', 'processing');
