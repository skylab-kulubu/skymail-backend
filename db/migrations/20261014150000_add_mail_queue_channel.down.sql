SET lock_timeout = '5s';

ALTER TABLE mail_queue
    DROP CONSTRAINT IF EXISTS mail_queue_channel_check,
    DROP COLUMN IF EXISTS recipient_sub,
    DROP COLUMN IF EXISTS channel;

RESET lock_timeout;
