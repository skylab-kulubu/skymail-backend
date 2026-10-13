-- Do not run (see the up migration): it drops the record of who opened a
-- sensitive send's content, and masked content stays masked. It restores
-- 20261012180000's mail_task_content_sensitive().
SET lock_timeout = '5s';

DROP TABLE IF EXISTS sent_content_views;
DROP TRIGGER IF EXISTS templates_mask_due ON templates;
DROP FUNCTION IF EXISTS mail_tasks_mask_due_on_template_change();

CREATE OR REPLACE FUNCTION mail_task_content_sensitive(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT COALESCE((SELECT t.content_sensitive
                 FROM mail_tasks mt
                          JOIN templates t ON t.id = mt.template_id
                 WHERE mt.id = for_task),
                NOT mail_task_bulk(for_task))
$$;

DROP FUNCTION IF EXISTS template_key_content_locked(TEXT);

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS content_mask_failures,
    DROP COLUMN IF EXISTS content_mask_due,
    DROP COLUMN IF EXISTS content_masked_variables,
    DROP COLUMN IF EXISTS content_masked_by,
    DROP COLUMN IF EXISTS content_masked_at,
    DROP COLUMN IF EXISTS content_sensitive_at_send;

RESET lock_timeout;
