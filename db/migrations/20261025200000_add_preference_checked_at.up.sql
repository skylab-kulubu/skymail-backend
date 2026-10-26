-- The bulk preference check (ADR 0072 decision 3; hub ticket
-- abonelik-cikisi 09; README, "Toplu postada tercih"): while
-- SKYMAIL_BULK_PREFERENCE is on, every duyuru send asks core about its
-- recipients before it is queued, and the dispatcher asks again right before
-- it sends.
--
--   preference_checked_at: when core last said this recipient may get the
--     mail (allowed, with the person's link). NULL for a row core gave no
--     verdict about: an address without an account (SkyMail's own link), a
--     notification (core's link came with it), any islem row, and every row
--     queued while the switch was off. The dispatcher asks again about the
--     rows that have one.
--
-- A recipient core says no to is written failed, nothing tried, with
-- 'preference: opted_out', 'preference: inactive' or 'preference: ambiguous':
-- skipped, as a suppressed one is, so it never makes a send read failed
-- (mail_task_status() below). 'preference: unavailable' (core never answered
-- for 48 hours) is not skipped: it was never decided.
--
-- Expand only (docs/database-migrations.md): a nullable column without a
-- default rewrites nothing, and an image from before names its columns, so it
-- leaves the column NULL. The function only skips more: an image from before
-- never writes these errors.
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN preference_checked_at TIMESTAMPTZ;

-- self_test: a send of a mail to its own sender's verified address, to see it
-- before the real one (a News send's test mail, POST /v1/news_sends/test).
-- The check neither asks core about it nor holds it for a missing link: it
-- reaches nobody else, and carries no List-Unsubscribe. A default that is a
-- constant rewrites nothing; an image from before writes false.
ALTER TABLE mail_tasks
    ADD COLUMN self_test BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN mail_tasks.self_test IS
    'A send to its own sender''s verified address alone (a test mail): the bulk preference check leaves it be.';

COMMENT ON COLUMN mail_queue.preference_checked_at IS
    'When core last allowed this recipient a duyuru mail (recipients:resolve); NULL when core gave no verdict about the row.';

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

RESET lock_timeout;
