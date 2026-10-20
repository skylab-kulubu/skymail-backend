-- 20261020150000's mail_task_status(): a suppressed recipient makes its send
-- failed again. The removal records and the flag go; suppressed rows stay
-- failed with their error.
SET lock_timeout = '5s';

CREATE OR REPLACE FUNCTION mail_task_status(for_task UUID) RETURNS TEXT
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT CASE
           WHEN EXISTS (SELECT 1
                        FROM mail_queue
                        WHERE task_id = for_task
                          AND status = 'failed'
                          AND COALESCE(error, '') NOT LIKE 'reserved\_domain:%')
               THEN 'failed'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status IN ('pending', 'processing'))
               THEN 'sending'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status = 'sent')
               THEN 'sent'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task)
               THEN 'failed'
           WHEN (SELECT created_at FROM mail_tasks WHERE id = for_task) > now() - INTERVAL '1 minute'
               THEN 'sending'
           ELSE 'failed'
           END
$$;

DROP TABLE mail_suppression_removals;

ALTER TABLE mail_queue
    DROP COLUMN suppressed_but_sent;

RESET lock_timeout;
