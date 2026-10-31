-- One send per News record and audience, a cancelled one aside (hub: Yusuf's
-- decision of 2026-10-31; review duyuru-haber B3): a send cancelled half way
-- no longer holds back the record to the same audience; sent again, it goes
-- only to who never got it (its cancelled rows carry no source_ref). The
-- next migration drops the index it replaces, idx_mail_tasks_news_audience.
--
-- CONCURRENTLY, alone in its file, as 20261025190200.
CREATE UNIQUE INDEX CONCURRENTLY idx_mail_tasks_news_audience_open
    ON mail_tasks (source_ref, source_audience)
    WHERE source_ref IS NOT NULL AND cancelled_at IS NULL;
