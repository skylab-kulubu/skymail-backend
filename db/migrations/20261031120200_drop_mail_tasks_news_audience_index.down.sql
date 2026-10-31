-- Puts the index back as 20261025190200 made it. It cannot be built once a
-- cancelled send of a record was sent again to the same audience (two sends
-- of one record and audience): such a down fails and leaves an INVALID index
-- (docs/database-migrations.md, dirty version).
CREATE UNIQUE INDEX CONCURRENTLY idx_mail_tasks_news_audience
    ON mail_tasks (source_ref, source_audience)
    WHERE source_ref IS NOT NULL;
