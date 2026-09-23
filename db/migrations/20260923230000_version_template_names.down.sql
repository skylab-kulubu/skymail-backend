-- Versions forget their names; the templates keep theirs, so no mail changes.
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
       COALESCE(v.id = t.published_version_id, false)::boolean AS is_current,
       v.discarded_at
FROM template_versions v
         JOIN templates t ON t.id = v.template_id;

ALTER TABLE template_versions
    DROP COLUMN IF EXISTS name;
