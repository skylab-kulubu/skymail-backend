-- "Bir kez" (hub ticket duyuru-haber 06, SPEC §5.7 madde 10): a News
-- record's mail goes to a mailbox once, whichever sends of it reach the
-- address. POST /v1/news_sends leaves out, under the record's lock, the
-- addresses a row of the record already went to; this index is the net
-- beneath it: a second row of the same record to the same mailbox cannot be
-- written. Partial: only rows of a News send carry source_ref, so it holds
-- nothing for the rest of the queue.
--
-- CONCURRENTLY, alone in its file, without lock_timeout, as
-- 20261012130100: it does not hold back the queue's writes while it reads
-- the table. A build that fails leaves an INVALID index of this name and the
-- version dirty: drop it, then put the version back
-- (docs/database-migrations.md, dirty version).
CREATE UNIQUE INDEX CONCURRENTLY idx_mail_queue_news_once
    ON mail_queue (source_ref, lower(recipient_email))
    WHERE source_ref IS NOT NULL;
