-- An operator can give up a draft. A discarded draft stays in its template's
-- history, readable and restorable like any version (ADR-0046: versions are
-- never deleted), but it is nobody's draft in progress any more and it is
-- never published. Without this, a draft its author walked away from would
-- stay "in progress" for good.
ALTER TABLE template_versions
    ADD COLUMN discarded_at TIMESTAMPTZ,
    ADD CONSTRAINT template_versions_discarded_unpublished CHECK (discarded_at IS NULL OR published_at IS NULL);

-- The summary gains when a version was discarded; its other columns stay as
-- they were, so the view is replaced in place.
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
       v.discarded_at
FROM template_versions v
         JOIN templates t ON t.id = v.template_id;
