CREATE TABLE mail_tasks
(
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sent_by        TEXT        NOT NULL,
    template_id    UUID        REFERENCES templates (id) ON DELETE SET NULL,
    mail_list_id   UUID        REFERENCES mailing_lists (id) ON DELETE SET NULL,
    body_variables JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TYPE mail_queue_status AS ENUM ('pending', 'processing', 'sent', 'failed');

CREATE TABLE mail_queue
(
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id             UUID NOT NULL REFERENCES mail_tasks (id) ON DELETE CASCADE,
    recipient_full_name TEXT NOT NULL,
    recipient_email     TEXT NOT NULL,
    subject             TEXT NOT NULL,
    body                TEXT NOT NULL,
    body_html           TEXT,
    status              mail_queue_status DEFAULT 'pending',
    error               TEXT,
    created_at          TIMESTAMPTZ       DEFAULT NOW()
);

CREATE INDEX idx_mail_queue_task_id ON mail_queue (task_id);
CREATE INDEX idx_mail_queue_active_jobs
    ON mail_queue (status, created_at)
    WHERE status IN ('pending', 'processing');
