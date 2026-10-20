-- A recipient in a domain reserved by RFC 2606/6761 (a sandbox seed account at
-- example.com) is written failed with 'reserved_domain: …' when its send is
-- queued, and is never sent (#82, WithReservedDomainsRefused). Such a row is
-- skipped, not failed: no mail could ever reach it, and until now one of them
-- made its whole send read failed while the real recipients were still being
-- sent, or all of them had been. mail_task_status() now leaves it out of the
-- failed probe:
--   failed   at least one recipient failed, other than a skipped one,
--            whatever the others did;
--   sending  otherwise, at least one recipient is still pending or processing;
--   sent     otherwise, at least one recipient was sent;
--   failed   otherwise, every recipient was skipped: nothing went out.
-- A task with no queue rows is as before (20260922235000). The row itself
-- stays failed with its error: the send's page shows it per recipient.
--
-- Only the function changes, so an image from before this one runs on it as
-- it is, and rolling back needs no down migration.
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

RESET lock_timeout;
