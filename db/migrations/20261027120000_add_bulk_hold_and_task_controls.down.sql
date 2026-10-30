SET lock_timeout = '5s';

-- A paused send's pending rows are parked far ahead (2999-01-01). Without
-- the columns nothing would ever resume them: they go back due now, and the
-- image from before sends them.
UPDATE mail_queue
SET next_attempt_at = now()
WHERE status = 'pending'
  AND next_attempt_at >= TIMESTAMPTZ '2999-01-01 00:00:00+00';

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
                          AND COALESCE(error, '') NOT LIKE 'reserved\_domain:%'
                          AND COALESCE(error, '') NOT LIKE 'suppressed:%'
                          AND COALESCE(error, '') NOT IN ('preference: opted_out', 'preference: inactive', 'preference: ambiguous'))
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

ALTER TABLE mail_tasks
    DROP CONSTRAINT IF EXISTS mail_tasks_paused_reason_check,
    DROP COLUMN IF EXISTS paused_at,
    DROP COLUMN IF EXISTS paused_reason,
    DROP COLUMN IF EXISTS paused_by,
    DROP COLUMN IF EXISTS send_failures,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS cancelled_by;

ALTER TABLE mail_queue
    DROP COLUMN IF EXISTS throttles;

DROP TABLE IF EXISTS mail_bulk_holds;

RESET lock_timeout;
