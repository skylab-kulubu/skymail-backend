-- Down drops the columns and functions. Content the sweep already emptied
-- stays empty: it is not kept anywhere else.
SET lock_timeout = '5s';

DROP FUNCTION IF EXISTS mail_task_content_sensitive(UUID);
DROP FUNCTION IF EXISTS mail_task_bulk(UUID);

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS content_cleared_at;

ALTER TABLE templates
    DROP COLUMN IF EXISTS content_sensitive;

RESET lock_timeout;
