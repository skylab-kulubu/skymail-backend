-- The sends whose tracking is still open, oldest first: what retention reads
-- to close a send's tracking 90 days on. A send leaves the index once closed.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE INDEX CONCURRENTLY idx_mail_tasks_tracking_open
    ON mail_tasks (created_at)
    WHERE tracking_requested AND tracking_closed_at IS NULL;
