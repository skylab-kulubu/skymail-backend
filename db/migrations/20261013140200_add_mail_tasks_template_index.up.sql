-- The sends of a template: what the templates_mask_due trigger
-- (20261013140000) reads when a template's content_sensitive or key changes,
-- so it does not read every send.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout: sends are
-- queued while it builds. The same recovery from a failed build as
-- 20261012130100 (docs/database-migrations.md, dirty version).
CREATE INDEX CONCURRENTLY idx_mail_tasks_template
    ON mail_tasks (template_id);
