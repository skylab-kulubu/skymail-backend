-- One send per News record and audience (hub ticket duyuru-haber 06): the
-- same record asked again for the same audience answers the send that
-- exists (409 news_send.already_sent), open or done. Also the index
-- GET /v1/mail_tasks?source=news:<slug> reads. Partial: only News sends carry
-- a source_ref.
--
-- CONCURRENTLY, alone in its file, as 20261012130100.
CREATE UNIQUE INDEX CONCURRENTLY idx_mail_tasks_news_audience
    ON mail_tasks (source_ref, source_audience)
    WHERE source_ref IS NOT NULL;
