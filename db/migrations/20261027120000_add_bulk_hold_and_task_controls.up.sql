-- The bulk lane's guards (hub ticket duyuru-haber 05, G3 and G4; README,
-- "Kısma ve görev düzeyinde işlemler").
--
--   mail_bulk_holds: one row, the instant until which no process takes a
--     bulk row. A throttling reply of the relay (SMTP 421, 432 or 4.7.x;
--     Graph 429 or 503) writes it: the relay asked the account to slow down,
--     so every replica's bulk lane waits SKYMAIL_THROTTLE_PAUSE (or the
--     reply's Retry-After, if longer). The system lane never reads it.
--
--   mail_tasks.paused_at, paused_reason, paused_by: a send whose rows the
--     bulk lane does not take until it is resumed. paused_reason is
--     'operator' (POST /v1/mail_tasks/{id}/pause) or 'failure_rate' (its
--     recipients failed past SKYMAIL_FAILURE_PAUSE_COUNT and
--     SKYMAIL_FAILURE_PAUSE_PERCENT; the listmonk pattern).
--   mail_tasks.send_failures: how many of the send's recipients failed for
--     good at the relay since it was queued or last resumed; what the
--     failure rate reads.
--   mail_queue.throttles: how many throttling replies a row had, none of
--     them counted as an attempt. Past a few (the mailer's maxRowThrottles)
--     the row's next one is retried as any other failure: a relay answering
--     one recipient 4.7.x for good never holds the bulk lane for ever.
--   mail_tasks.cancelled_at, cancelled_by: a cancelled send. Its pending
--     rows are written failed with 'cancelled', nothing tried: skipped, so
--     they never make the send read failed (mail_task_status() below). Rows
--     being sent are left to finish.
--
-- Expand only (docs/database-migrations.md): a new table, nullable columns
-- and a column whose default is a constant rewrite nothing; an image from
-- before names its columns and never reads them. Its bulk lane takes a paused
-- send's rows while it still runs, as it did before.
SET lock_timeout = '5s';

CREATE TABLE mail_bulk_holds
(
    id         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    held_until TIMESTAMPTZ NOT NULL,
    reason     TEXT        NOT NULL,
    held_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE mail_bulk_holds IS
    'One row: the bulk lane takes nothing before held_until (a throttling reply of the relay).';

ALTER TABLE mail_tasks
    ADD COLUMN paused_at     TIMESTAMPTZ,
    ADD COLUMN paused_reason TEXT,
    ADD COLUMN paused_by     TEXT,
    ADD COLUMN send_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN cancelled_at  TIMESTAMPTZ,
    ADD COLUMN cancelled_by  TEXT;

ALTER TABLE mail_queue
    ADD COLUMN throttles INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN mail_queue.throttles IS
    'Throttling replies the row had with no attempt counted; past a few its attempts count.';

ALTER TABLE mail_tasks
    ADD CONSTRAINT mail_tasks_paused_reason_check
        CHECK (paused_reason IN ('operator', 'failure_rate')) NOT VALID;

COMMENT ON COLUMN mail_tasks.paused_at IS
    'While set, the bulk lane takes none of the send''s rows (paused by an operator or by its failure rate).';
COMMENT ON COLUMN mail_tasks.cancelled_at IS
    'A cancelled send: its pending rows were written failed with ''cancelled''.';

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
                          AND COALESCE(error, '') NOT IN ('preference: opted_out', 'preference: inactive', 'preference: ambiguous', 'cancelled'))
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
