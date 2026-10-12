-- The sends whose content is still kept, oldest first: what the content sweep
-- reads every few minutes (ClearDueMailContent). A send leaves the index when
-- the sweep empties it, so a sweep reads the sends of the retention period
-- and those still going out, never the club's whole history.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE INDEX CONCURRENTLY idx_mail_tasks_content_kept
    ON mail_tasks (created_at)
    WHERE content_cleared_at IS NULL;
