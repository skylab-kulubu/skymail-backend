-- Rows already erased stay erased; only the flag and the trigger go.
SET lock_timeout = '5s';

DROP TRIGGER IF EXISTS mail_queue_erase_after_send ON mail_queue;
DROP FUNCTION IF EXISTS mail_queue_erase_after_send();
ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS erase_after_send;

RESET lock_timeout;
