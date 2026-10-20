-- Do not run (see the up migration). It gives back 20261013140000's
-- mail_task_content_sensitive() and its column comment; the copies the up
-- migration and the image wrote stay 'true', which that function reads as
-- sensitive too.
SET lock_timeout = '5s';

CREATE OR REPLACE FUNCTION mail_task_content_sensitive(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT COALESCE((SELECT template_key_content_locked(t.key)
                            OR COALESCE(t.content_sensitive, NOT mail_task_bulk(mt.id))
                            OR CASE mt.content_sensitive_at_send
                                   WHEN 'true' THEN true
                                   WHEN 'false' THEN false
                                   ELSE NOT mail_task_bulk(mt.id) END
                 FROM mail_tasks mt
                          LEFT JOIN templates t ON t.id = mt.template_id
                 WHERE mt.id = for_task),
                true)
$$;

COMMENT ON COLUMN mail_tasks.content_sensitive_at_send IS
    'The template''s content_sensitive when the send was queued: true, false or unset (NULL). A send is sensitive if this or its template now says so.';

RESET lock_timeout;
