SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS notification_id;
DROP TABLE IF EXISTS notifications;
ALTER TABLE templates
    DROP COLUMN IF EXISTS channel;
DROP TABLE IF EXISTS notification_types;

RESET lock_timeout;
