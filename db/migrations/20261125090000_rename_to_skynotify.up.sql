-- SkyNotify (ADR 0091): the service is renamed in one window, and nothing in
-- the schema keeps the old name.
--
--   skynotify_search_fold(): the list searches' fold under its new name, made
--     anew exactly as 20261124200000 made it (no search_path, so it is
--     inlined; see there); the old one is dropped. Nothing else calls it, no
--     index or view is built on it.
--   The comments that named the service or its settings name them anew.
--
-- Not expand only: an image from before calls the old function. The rename
-- window stops it before this runs; going back, db/tasima/sema-geri.sql (or
-- this migration's down) restores the old schema before the old image starts.
-- The stored format markers in the rows are not a migration: db/tasima/
-- veri-ileri.sql moves them, before this image starts.
SET lock_timeout = '5s';

CREATE FUNCTION skynotify_search_fold(value TEXT) RETURNS TEXT
    LANGUAGE sql
    IMMUTABLE
    PARALLEL SAFE
AS
$$
SELECT pg_catalog.lower(pg_catalog.translate(value, 'İIıŞĞÜÖÇ', 'iiişğüöç'))
$$;

DROP FUNCTION skymail_search_fold(TEXT);

COMMENT ON COLUMN mail_tasks.tracking_requested IS
    'The sender asked for open and click tracking and SKYNOTIFY_TRACKING was on when the send was queued.';
COMMENT ON COLUMN mail_approvals.tracking_requested IS
    'The submitter asked for open and click tracking; the approved send asks for it if SKYNOTIFY_TRACKING is on then.';
COMMENT ON TABLE mail_unsubscribes IS
    'Addresses that left a broadcast stream (duyuru) through SkyNotify''s link: that stream''s mail is not sent to them. Removed by the person, by an operator at their request, or by Account erasure; never by itself.';
COMMENT ON COLUMN mail_tasks.sent_by_service IS
    'Whether a service or SkyNotify itself queued the send (a service account''s single mail, SkyNotify''s notices); false for a person''s single mail; NULL before 20261124200000 and where the kind does not need it.';

RESET lock_timeout;
