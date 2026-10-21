SET lock_timeout = '5s';

ALTER TABLE mail_queue
    DROP CONSTRAINT IF EXISTS mail_queue_list_unsubscribe_url_check,
    DROP COLUMN IF EXISTS list_unsubscribe_url;

RESET lock_timeout;
