DROP INDEX IF EXISTS idx_mailing_lists_archived_at;
DROP INDEX IF EXISTS idx_mailing_lists_current_created_at;
DROP INDEX IF EXISTS idx_templates_archived_at;
DROP INDEX IF EXISTS idx_templates_current_created_at;

ALTER TABLE mailing_lists
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;

ALTER TABLE templates
    DROP COLUMN IF EXISTS archived_by,
    DROP COLUMN IF EXISTS archived_at;
