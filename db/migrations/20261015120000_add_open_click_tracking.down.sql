SET lock_timeout = '5s';

DROP TABLE IF EXISTS mail_task_tracking_totals;
DROP TABLE IF EXISTS mail_tracking_events;
DROP TABLE IF EXISTS mail_task_links;

ALTER TABLE mail_queue
    DROP CONSTRAINT IF EXISTS mail_queue_tracking_skip_reason_value,
    DROP CONSTRAINT IF EXISTS mail_queue_tracking_token_length,
    DROP COLUMN IF EXISTS sent_at,
    DROP COLUMN IF EXISTS tracking_skip_reason,
    DROP COLUMN IF EXISTS tracking_disabled_at,
    DROP COLUMN IF EXISTS tracking_token;

ALTER TABLE mail_approvals
    DROP COLUMN IF EXISTS tracking_requested;

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS tracking_closed_at,
    DROP COLUMN IF EXISTS tracking_requested;

RESET lock_timeout;
