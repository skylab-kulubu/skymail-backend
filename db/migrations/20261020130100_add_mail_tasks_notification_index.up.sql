-- The e-mail send a notification queued (POST /v1/notifications), for the
-- send list and the per-type counts.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout: sends are
-- queued while it builds. The same recovery from a failed build as
-- 20261012130100 (docs/database-migrations.md, dirty version).
CREATE INDEX CONCURRENTLY idx_mail_tasks_notification
    ON mail_tasks (notification_id) WHERE notification_id IS NOT NULL;
