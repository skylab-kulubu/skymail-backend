-- A template's name is part of its versions (ADR-0046: every change is a
-- version). Until now a rename in the old panel changed only the row and wrote
-- no version, so the Template seed, which writes the name too, put the repo's
-- name back over an operator's rename with nothing to show for it, and no
-- conflict rule could see the rename. From here on every version records the
-- name, publishing copies it onto the row with the rest, and a rename is an
-- operator's version like any other change.
--
-- Which name each existing version had is not known; they take the name the
-- template has now, the closest the row comes to it.
ALTER TABLE template_versions
    ADD COLUMN name TEXT;

UPDATE template_versions v
SET name = t.name
FROM templates t
WHERE t.id = v.template_id;

ALTER TABLE template_versions
    ALTER COLUMN name SET NOT NULL;

-- The summary gains the name; its other columns stay as they were, so the view
-- is replaced in place.
CREATE OR REPLACE VIEW template_version_summaries AS
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
       v.discarded_at,
       v.name
FROM template_versions v
         JOIN templates t ON t.id = v.template_id;
