-- The rows keep the published copy the send path reads, so going down changes
-- no mail. It does drop the history: every version, drafts included, goes with
-- the table.
DROP VIEW IF EXISTS template_version_summaries;

ALTER TABLE templates
    DROP CONSTRAINT IF EXISTS templates_published_version_same_template,
    DROP COLUMN IF EXISTS published_version_id;

DROP TABLE IF EXISTS template_versions;

DROP FUNCTION IF EXISTS template_jsx_source(TEXT);

DROP TYPE IF EXISTS template_author_kind;

DROP TYPE IF EXISTS authoring_mode;
