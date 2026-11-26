-- The schema as 20261124200000 left it: the old fold function and comments,
-- byte for byte (db/tasima/sema-geri.sql is this file and the version).
SET lock_timeout = '5s';

CREATE FUNCTION skymail_search_fold(value TEXT) RETURNS TEXT
    LANGUAGE sql
    IMMUTABLE
    PARALLEL SAFE
AS
$$
SELECT pg_catalog.lower(pg_catalog.translate(value, 'İIıŞĞÜÖÇ', 'iiişğüöç'))
$$;

DROP FUNCTION skynotify_search_fold(TEXT);

COMMENT ON COLUMN mail_tasks.tracking_requested IS
    'The sender asked for open and click tracking and SKYMAIL_TRACKING was on when the send was queued.';
COMMENT ON COLUMN mail_approvals.tracking_requested IS
    'The submitter asked for open and click tracking; the approved send asks for it if SKYMAIL_TRACKING is on then.';
COMMENT ON TABLE mail_unsubscribes IS
    'Addresses that left a broadcast stream (duyuru) through SkyMail''s link: that stream''s mail is not sent to them. Removed by the person, by an operator at their request, or by Account erasure; never by itself.';
COMMENT ON COLUMN mail_tasks.sent_by_service IS
    'Whether a service or SkyMail itself queued the send (a service account''s single mail, SkyMail''s notices); false for a person''s single mail; NULL before 20261124200000 and where the kind does not need it.';

RESET lock_timeout;
