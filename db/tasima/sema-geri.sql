-- SkyNotify ad taşıması (ADR 0091, hub bileti skynotify-ad 02): şema geri göçü.
--
-- db/migrations/20261125090000_rename_to_skynotify.down.sql ile aynı şemayı
-- kurar ve schema_migrations'ı önceki sürüme (20261124200000, dirty=false)
-- getirir: eski imaj açılışta göç bulmaz, kendi şemasını bulur. Pencerenin
-- geri alma adımı 2'de, eski imaj başlamadan önce koşar (DB hâlâ skynotify
-- adındayken; veri geri göçü veri-geri.sql ayrıca):
--
--   docker exec -i <konteyner> psql -X -v ON_ERROR_STOP=1 -1 -U "$POSTGRES_USER" -d skynotify -f - < sema-geri.sql
--
-- -1 zorunludur: onsuz ilk denetim durur, hiçbir şey değişmez.
--
-- Yalnız sürüm 20261125090000'da koşar. Yeni imajın göçü yarıda kalmışsa
-- (dirty=true) işlemi geri alınmıştır, şema zaten eskidir: dosya yine aynı
-- yere getirir (CREATE OR REPLACE, IF EXISTS). Sürüm başka bir şeyse hiçbir
-- şeye dokunmadan durur; zaten geri alınmış bir şemada (20261124200000) da.
--
-- Çıktı: sonuc|sema-geri|<sürüm>|<dirty>.
\set ON_ERROR_STOP on
\set QUIET on
\pset format unaligned
\pset tuples_only on
\pset fieldsep '|'
\pset footer off
\pset pager off

SET LOCAL lock_timeout = '5s';

-- ON COMMIT DROP: without -1 it is gone by the next statement, and the check
-- below stops before anything is written.
CREATE TEMP TABLE tasima_sema_geri (x INT) ON COMMIT DROP;

DO
$tasima$
    DECLARE
        satir INT;
        surum BIGINT;
        kirli BOOLEAN;
    BEGIN
        PERFORM FROM tasima_sema_geri;
        SELECT count(*), max(version), bool_or(dirty) INTO satir, surum, kirli FROM schema_migrations;
        IF satir <> 1 OR surum IS DISTINCT FROM 20261125090000 THEN
            RAISE EXCEPTION 'schema_migrations: % satır, sürüm %, dirty %; beklenen tek satır, sürüm 20261125090000', satir, surum, kirli
                USING HINT = 'Hiçbir şey değişmedi. 20261124200000 ise şema zaten geri alınmış.';
        END IF;
        IF NOT kirli AND to_regprocedure('skynotify_search_fold(text)') IS NULL THEN
            RAISE EXCEPTION 'sürüm 20261125090000 temiz ama skynotify_search_fold(text) yok'
                USING HINT = 'Hiçbir şey değişmedi; şemaya elle bakılmalı.';
        END IF;
    END
$tasima$;

CREATE OR REPLACE FUNCTION skymail_search_fold(value TEXT) RETURNS TEXT
    LANGUAGE sql
    IMMUTABLE
    PARALLEL SAFE
AS
$$
SELECT pg_catalog.lower(pg_catalog.translate(value, 'İIıŞĞÜÖÇ', 'iiişğüöç'))
$$;

DROP FUNCTION IF EXISTS skynotify_search_fold(TEXT);

COMMENT ON COLUMN mail_tasks.tracking_requested IS
    'The sender asked for open and click tracking and SKYMAIL_TRACKING was on when the send was queued.';
COMMENT ON COLUMN mail_approvals.tracking_requested IS
    'The submitter asked for open and click tracking; the approved send asks for it if SKYMAIL_TRACKING is on then.';
COMMENT ON TABLE mail_unsubscribes IS
    'Addresses that left a broadcast stream (duyuru) through SkyMail''s link: that stream''s mail is not sent to them. Removed by the person, by an operator at their request, or by Account erasure; never by itself.';
COMMENT ON COLUMN mail_tasks.sent_by_service IS
    'Whether a service or SkyMail itself queued the send (a service account''s single mail, SkyMail''s notices); false for a person''s single mail; NULL before 20261124200000 and where the kind does not need it.';

UPDATE schema_migrations
SET version = 20261124200000,
    dirty   = false;

DO
$tasima$
    BEGIN
        IF (SELECT count(*) FROM schema_migrations WHERE version = 20261124200000 AND NOT dirty) <> 1
            OR (SELECT count(*) FROM schema_migrations) <> 1
            OR to_regprocedure('skymail_search_fold(text)') IS NULL
            OR to_regprocedure('skynotify_search_fold(text)') IS NOT NULL THEN
            RAISE EXCEPTION 'şema geri göçü tutmadı, geri alındı';
        END IF;
    END
$tasima$;

SELECT 'sonuc', 'sema-geri', version, dirty FROM schema_migrations;
