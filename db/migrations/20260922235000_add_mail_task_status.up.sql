-- A mail task has no status of its own; it has one queue row per recipient.
-- This is the one definition of the status a send is shown with and filtered
-- by, so the home screen and the send list can never disagree:
--   failed   at least one recipient failed, whatever the others did;
--   sending  otherwise, at least one recipient is still pending or processing;
--   sent     otherwise, at least one recipient was sent;
--   empty    the task has no recipient rows at all.
CREATE FUNCTION mail_task_status(for_task UUID) RETURNS TEXT
    LANGUAGE sql
    STABLE
AS
$$
SELECT CASE
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status = 'failed')
               THEN 'failed'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status IN ('pending', 'processing'))
               THEN 'sending'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status = 'sent')
               THEN 'sent'
           ELSE 'empty'
           END
$$;

-- Each probe above, and a send's per-status recipient counts, is an index
-- lookup on (task_id, status) rather than a walk over the task's rows.
CREATE INDEX idx_mail_queue_task_id_status ON mail_queue (task_id, status);

-- The daily sent series reads a range of this; the queue-wide sent and failed
-- counts are index-only scans of these two.
CREATE INDEX idx_mail_queue_sent_created_at ON mail_queue (created_at) WHERE status = 'sent';
CREATE INDEX idx_mail_queue_failed_created_at ON mail_queue (created_at) WHERE status = 'failed';

-- The send list and the recent sends are newest first; the id breaks ties so
-- sends made in the same instant keep one order from page to page.
CREATE INDEX idx_mail_tasks_created_at_id ON mail_tasks (created_at DESC, id DESC);
