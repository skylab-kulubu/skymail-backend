-- Discarded drafts become plain drafts again; the newest version of an
-- operator's that was discarded is their draft in progress once more.
DROP VIEW IF EXISTS template_version_summaries;

CREATE VIEW template_version_summaries AS
SELECT v.id,
       v.template_id,
       v.seq,
       v.subject,
       v.requested_subject,
       v.main_mode,
       v.author_kind,
       v.author_sub,
       v.author_name,
       v.created_at,
       v.published_at,
       v.base_version_id,
       COALESCE(v.id = t.published_version_id, false)::boolean AS is_current
FROM template_versions v
         JOIN templates t ON t.id = v.template_id;

ALTER TABLE template_versions
    DROP CONSTRAINT IF EXISTS template_versions_discarded_unpublished,
    DROP COLUMN IF EXISTS discarded_at;
