-- Drops the unique index that held a cancelled News send against the record
-- sent again to the same audience; idx_mail_tasks_news_audience_open
-- (20261031120100) holds the rest. An image from before reads no index by
-- name: its POST /v1/news_sends checks the audience's send under the
-- record's lock (LockNewsSource), so it still answers 409 for a cancelled
-- send, as it did.
--
-- CONCURRENTLY, alone in its file, as 20261025190200.
DROP INDEX CONCURRENTLY IF EXISTS idx_mail_tasks_news_audience;
