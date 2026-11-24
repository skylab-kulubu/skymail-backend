-- The send list's kind and search (GET /v1/mail_tasks, hub ticket
-- skymail-yeni-tasarim 16, B1).
--
--   mail_tasks.sent_by_service: whether a service, or SkyMail itself, queued
--     the send: a service account's POST /v1/mail_tasks/single (a login code,
--     a ticket, a Forms confirmation), SkyMail's own Mail onayı notices.
--     false for a person's single mail; NULL for every send queued before
--     this column, and for the sends whose kind does not need it (a list, a
--     group, a Forms source, an approved send, a News send, a core
--     notification).
--   mail_task_kind(): bulk (mail_task_bulk(), or a News send), system (a core
--     notification, or a send a service queued; one from before
--     sent_by_service by its lane: a row in the system lane, as
--     TrackingTaskAccess's system_lane reads it), single (any other). The
--     lane alone is not the rule: until every send is tracked, a person's
--     untracked single mail goes in the system lane too.
--   skymail_search_fold(): text as the list searches compare it: Turkish
--     capitals to their own small letters (İ, Ş, Ğ, Ü, Ö, Ç), whatever the
--     database's collation, and the four i's (I, ı, İ, i) as one, so IŞIK
--     finds ışık and Işık; then lower().
--
-- Expand only (docs/database-migrations.md): a nullable column is added
-- without a rewrite, an image from before names its columns and never reads
-- it; the functions are new.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN sent_by_service BOOLEAN;

COMMENT ON COLUMN mail_tasks.sent_by_service IS
    'Whether a service or SkyMail itself queued the send (a service account''s single mail, SkyMail''s notices); false for a person''s single mail; NULL before 20261124200000 and where the kind does not need it.';

CREATE FUNCTION mail_task_kind(for_task UUID) RETURNS TEXT
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT CASE
           WHEN mail_task_bulk(mt.id) OR mt.source_kind IS NOT NULL
               THEN 'bulk'
           WHEN mt.notification_id IS NOT NULL
               OR COALESCE(mt.sent_by_service,
                           EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = mt.id AND q.priority > 0))
               THEN 'system'
           ELSE 'single'
           END
FROM mail_tasks mt
WHERE mt.id = for_task
$$;

CREATE FUNCTION skymail_search_fold(value TEXT) RETURNS TEXT
    LANGUAGE sql
    IMMUTABLE
    PARALLEL SAFE
    SET search_path = pg_catalog, public
AS
$$
SELECT lower(translate(value, 'İIıŞĞÜÖÇ', 'iiişğüöç'))
$$;

RESET lock_timeout;
