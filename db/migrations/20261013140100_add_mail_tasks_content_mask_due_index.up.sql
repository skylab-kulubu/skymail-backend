-- The sends the content sweep still has to decide on masking (content_mask_due,
-- 20261013140000), oldest first: what ListSendsToMask reads every few
-- minutes. A send leaves the index once masked, emptied or found not
-- sensitive, so the sweep no longer reads every send it kept.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE INDEX CONCURRENTLY idx_mail_tasks_content_mask_due
    ON mail_tasks (created_at)
    WHERE content_mask_due;
