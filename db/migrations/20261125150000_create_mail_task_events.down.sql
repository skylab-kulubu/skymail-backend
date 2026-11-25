-- The events go; the sends stay. An image from before writes none.
SET lock_timeout = '5s';

DROP TABLE IF EXISTS mail_task_events;

RESET lock_timeout;
