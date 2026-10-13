SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS content_mask_unconfirmed,
    DROP COLUMN IF EXISTS content_mask_retry_at;

RESET lock_timeout;
