ALTER TABLE templates
    ADD COLUMN archived_at TIMESTAMPTZ,
    ADD COLUMN archived_by TEXT;

ALTER TABLE mailing_lists
    ADD COLUMN archived_at TIMESTAMPTZ,
    ADD COLUMN archived_by TEXT;

CREATE INDEX idx_templates_current_created_at
    ON templates (created_at DESC)
    WHERE archived_at IS NULL;

CREATE INDEX idx_templates_archived_at
    ON templates (archived_at DESC)
    WHERE archived_at IS NOT NULL;

CREATE INDEX idx_mailing_lists_current_created_at
    ON mailing_lists (created_at DESC)
    WHERE archived_at IS NULL;

CREATE INDEX idx_mailing_lists_archived_at
    ON mailing_lists (archived_at DESC)
    WHERE archived_at IS NOT NULL;
