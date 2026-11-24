SET lock_timeout = '5s';

DROP FUNCTION IF EXISTS skymail_search_fold(TEXT);
DROP FUNCTION IF EXISTS mail_task_kind(UUID);
ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS sent_by_service;

RESET lock_timeout;
