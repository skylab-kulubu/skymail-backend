-- SkyNotify ad taşıması (ADR 0091, hub bileti skynotify-ad 02): veri göçü, geri.
--
-- veri-ileri.sql'in tersi, pencerenin geri alma adımı 2'de, eski imaj
-- başlamadan önce (DB hâlâ skynotify adındayken; şema geri göçü
-- sema-geri.sql ayrıca koşar):
--
--   template_versions.visual_source üst düzey "type": skynotify.visual → skymail.visual;
--   data-skynotify-notrack → data-skymail-notrack, veri-ileri.sql'in sütunlarında;
--   mail_tasks.sent_by = 'skynotify' → 'skymail'.
--
--   docker exec -i <konteyner> psql -X -v ON_ERROR_STOP=1 -1 -U "$POSTGRES_USER" -d skynotify -f - < veri-geri.sql
--
-- -1 zorunludur: onsuz ilk denetim durur, hiçbir satır değişmez.
--
-- Denetimler veri-ileri.sql'inkilerin simetriğidir: önce eski adlı kalıpların
-- hiçbiri yok (yeni imaj eski biçimde bir şey yazmamış olmalı), taşınacak
-- kalıp başka harf büyüklüğüyle ya da taşınmayan bir sütunda yok, Visual türü
-- yalnız üst düzeyde, tetikleyicinin sileceği satır yok; sonra her sütunda
-- taşınan kalıp 0, eski adlı kalıbın sayıları öncekine eşit, tablo satır
-- sayıları aynı. Tetikleyici kuralları veri-ileri.sql'deki gibidir.
--
-- Çıktı veri-ileri.sql'in biçiminde; en son satır sonuc|geri|tamam. Gövde
-- veri-ileri.sql'in aynısıdır (internal/migrations/skynotify_tasima_test.go
-- denetler); yalnız aşağıdaki değişkenler yer değiştirir: burada "eski",
-- taşınacak olan, yani SkyNotify adlısıdır.
\set ON_ERROR_STOP on
\set QUIET on
\pset format unaligned
\pset tuples_only on
\pset fieldsep '|'
\pset footer off
\pset pager off

\set yon 'geri'
\set eski_tur 'skynotify.visual'
\set yeni_tur 'skymail.visual'
\set eski_notrack 'data-skynotify-notrack'
\set yeni_notrack 'data-skymail-notrack'
\set eski_gonderen 'skynotify'
\set yeni_gonderen 'skymail'

-- ===== ortak gövde: veri-ileri.sql ile veri-geri.sql'de aynı =====
SET LOCAL lock_timeout = '5s';
SET LOCAL tasima.yon = :'yon';
SET LOCAL tasima.eski_tur = :'eski_tur';
SET LOCAL tasima.yeni_tur = :'yeni_tur';
SET LOCAL tasima.eski_notrack = :'eski_notrack';
SET LOCAL tasima.yeni_notrack = :'yeni_notrack';
SET LOCAL tasima.eski_gonderen = :'eski_gonderen';
SET LOCAL tasima.yeni_gonderen = :'yeni_gonderen';

-- Each moved column and marker: rows holding it, occurrences, rows holding it
-- in another letter case. The same query as veri-olcum.sql's.
CREATE TEMP VIEW tasima_sayim AS
-- sayım başı
WITH kalip(aile, yon, metin) AS (
    VALUES ('tur', 'eski', :'eski_tur'),
           ('tur', 'yeni', :'yeni_tur'),
           ('notrack', 'eski', :'eski_notrack'),
           ('notrack', 'yeni', :'yeni_notrack'),
           ('gonderen', 'eski', :'eski_gonderen'),
           ('gonderen', 'yeni', :'yeni_gonderen')),
     kapsam(sira, tablo, sutun, aile) AS (
         VALUES (1, 'templates', 'html_content', 'notrack'),
                (2, 'templates', 'react_email_content', 'notrack'),
                (3, 'template_versions', 'jsx_source', 'notrack'),
                (4, 'template_versions', 'html_source', 'notrack'),
                (5, 'template_versions', 'html_content', 'notrack'),
                (6, 'template_versions', 'visual_source', 'tur'),
                (7, 'template_versions', 'visual_source', 'notrack'),
                (8, 'mail_approvals', 'body_variables', 'notrack'),
                (9, 'mail_approval_events', 'changes', 'notrack'),
                (10, 'mail_tasks', 'body_variables', 'notrack'),
                (11, 'mail_tasks', 'source_snapshot', 'notrack'),
                (12, 'mail_tasks', 'sent_by', 'gonderen'),
                (13, 'mail_queue', 'body_html', 'notrack')),
     deger(tablo, sutun, icerik) AS (
         SELECT 'templates', 'html_content', html_content FROM templates
         UNION ALL SELECT 'templates', 'react_email_content', react_email_content FROM templates
         UNION ALL SELECT 'template_versions', 'jsx_source', jsx_source FROM template_versions
         UNION ALL SELECT 'template_versions', 'html_source', html_source FROM template_versions
         UNION ALL SELECT 'template_versions', 'html_content', html_content FROM template_versions
         UNION ALL SELECT 'template_versions', 'visual_source', visual_source::text FROM template_versions
         UNION ALL SELECT 'mail_approvals', 'body_variables', body_variables::text FROM mail_approvals
         UNION ALL SELECT 'mail_approval_events', 'changes', changes::text FROM mail_approval_events
         UNION ALL SELECT 'mail_tasks', 'body_variables', body_variables::text FROM mail_tasks
         UNION ALL SELECT 'mail_tasks', 'source_snapshot', source_snapshot::text FROM mail_tasks
         UNION ALL SELECT 'mail_tasks', 'sent_by', sent_by FROM mail_tasks
         UNION ALL SELECT 'mail_queue', 'body_html', body_html FROM mail_queue),
     eslesen(tablo, sutun, aile, yon, metin, icerik) AS (
         SELECT k.tablo, k.sutun, k.aile, p.yon, p.metin, d.icerik
         FROM deger d
                  JOIN kapsam k ON k.tablo = d.tablo AND k.sutun = d.sutun
                  JOIN kalip p ON p.aile = k.aile
         WHERE CASE k.aile
                   WHEN 'gonderen' THEN d.icerik = p.metin
                   ELSE d.icerik ILIKE '%' || p.metin || '%'
                   END)
SELECT k.sira, k.tablo, k.sutun, k.aile, p.yon, p.metin AS kalip,
       count(e.icerik) FILTER (WHERE e.aile = 'gonderen' OR strpos(e.icerik, e.metin) > 0) AS satir,
       coalesce(sum(CASE
                         WHEN e.aile = 'gonderen' THEN 1
                         ELSE (length(e.icerik) - length(replace(e.icerik, e.metin, ''))) / length(e.metin)
                         END), 0) AS adet,
       count(e.icerik) FILTER (WHERE e.aile <> 'gonderen' AND replace(e.icerik, e.metin, '') ILIKE '%' || e.metin || '%') AS harf
FROM kapsam k
         JOIN kalip p ON p.aile = k.aile
         LEFT JOIN eslesen e ON e.tablo = k.tablo AND e.sutun = k.sutun AND e.aile = k.aile AND e.yon = p.yon
GROUP BY k.sira, k.tablo, k.sutun, k.aile, p.yon, p.metin
-- sayım sonu
;

CREATE TEMP VIEW tasima_satir AS
SELECT 'templates' AS tablo, count(*) AS satir FROM templates
UNION ALL SELECT 'template_versions', count(*) FROM template_versions
UNION ALL SELECT 'mail_approvals', count(*) FROM mail_approvals
UNION ALL SELECT 'mail_approval_events', count(*) FROM mail_approval_events
UNION ALL SELECT 'mail_tasks', count(*) FROM mail_tasks
UNION ALL SELECT 'mail_queue', count(*) FROM mail_queue;

-- ON COMMIT DROP: without -1 these are gone by the next statement, and the
-- check below stops before anything is written.
CREATE TEMP TABLE tasima_once ON COMMIT DROP AS SELECT * FROM tasima_sayim;
CREATE TEMP TABLE tasima_satir_once ON COMMIT DROP AS SELECT * FROM tasima_satir;
CREATE TEMP TABLE tasima_guncellenen (sira INT, tablo TEXT, sutun TEXT, satir BIGINT) ON COMMIT DROP;

DO
$tasima$
    DECLARE
        eski_tur      TEXT   := current_setting('tasima.eski_tur');
        eski_notrack  TEXT   := current_setting('tasima.eski_notrack');
        eski_gonderen TEXT   := current_setting('tasima.eski_gonderen');
        sorun         TEXT[] := '{}';
        r             RECORD;
        ust_duzey     BIGINT;
        tur_adet      BIGINT;
        silinecek     BIGINT;
        sorgu         TEXT;
        bulunan       TEXT[];
    BEGIN
        -- The markers go into LIKE patterns as they are.
        IF concat(eski_tur, eski_notrack, eski_gonderen, current_setting('tasima.yeni_tur'),
                  current_setting('tasima.yeni_notrack'), current_setting('tasima.yeni_gonderen')) ~ '[%_\\]' THEN
            RAISE EXCEPTION 'a marker holds a LIKE wildcard';
        END IF;

        -- The new markers are nowhere yet, in any letter case: going back then
        -- undoes exactly what this does.
        FOR r IN SELECT * FROM tasima_once WHERE yon = 'yeni' AND (satir > 0 OR harf > 0) ORDER BY sira
            LOOP
                sorun := sorun || format('%s.%s: yeni kalıp %s zaten %s satırda', r.tablo, r.sutun, r.kalip, r.satir + r.harf);
            END LOOP;
        -- An old marker in another letter case would be left behind.
        FOR r IN SELECT * FROM tasima_once WHERE yon = 'eski' AND harf > 0 ORDER BY sira
            LOOP
                sorun := sorun || format('%s.%s: eski kalıp %s başka harf büyüklüğüyle %s satırda', r.tablo, r.sutun, r.kalip, r.harf);
            END LOOP;

        -- The Visual type moves only as the document's top-level type.
        SELECT count(*) INTO ust_duzey FROM template_versions WHERE visual_source ->> 'type' = eski_tur;
        SELECT adet INTO tur_adet FROM tasima_once WHERE aile = 'tur' AND yon = 'eski';
        IF tur_adet IS DISTINCT FROM ust_duzey THEN
            sorun := sorun || format('template_versions.visual_source: %s %s kez geçiyor, %s belgenin üst düzey türü',
                                     eski_tur, tur_adet, ust_duzey);
        END IF;

        -- mail_queue_erase_after_send would empty such a row as it is updated.
        SELECT count(*)
        INTO silinecek
        FROM mail_queue q
                 JOIN mail_tasks t ON t.id = q.task_id
        WHERE t.erase_after_send
          AND q.status IN ('sent', 'failed')
          AND q.body_html LIKE '%' || eski_notrack || '%';
        IF silinecek > 0 THEN
            sorun := sorun || format('mail_queue.body_html: %s satır erase_after_send bir gönderimin, tetikleyici silerdi', silinecek);
        END IF;

        -- No old marker in a text or JSON column this does not move.
        FOR sorgu IN
            SELECT format('SELECT array_remove(ARRAY[%s]::TEXT[], NULL) FROM %I',
                          string_agg(format('CASE WHEN count(*) FILTER (WHERE %s) > 0 THEN %L END',
                                            CASE a.aile
                                                WHEN 'gonderen' THEN format('%I::text = %L', c.column_name, a.metin)
                                                ELSE format('strpos(%I::text, %L) > 0', c.column_name, a.metin)
                                                END,
                                            format('%s.%s: taşınmayan sütunda eski kalıp %s', c.table_name, c.column_name, a.metin)),
                                     ', ' ORDER BY c.ordinal_position, a.aile),
                          c.table_name)
            FROM information_schema.columns c
                     JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
                     CROSS JOIN (VALUES ('tur', eski_tur), ('notrack', eski_notrack), ('gonderen', eski_gonderen)) a(aile, metin)
            WHERE c.table_schema = 'public'
              AND t.table_type = 'BASE TABLE'
              AND c.table_name <> 'schema_migrations'
              AND (c.data_type IN ('text', 'character varying', 'character', 'json', 'jsonb')
                OR c.udt_name IN ('_text', '_varchar'))
              AND NOT EXISTS (SELECT 1
                              FROM tasima_once o
                              WHERE o.tablo = c.table_name
                                AND o.sutun = c.column_name
                                AND o.aile = a.aile)
            GROUP BY c.table_name
            ORDER BY c.table_name
            LOOP
                EXECUTE sorgu INTO bulunan;
                sorun := sorun || bulunan;
            END LOOP;

        IF cardinality(sorun) > 0 THEN
            RAISE EXCEPTION 'SkyNotify veri göçü (%) başlamadı: %', current_setting('tasima.yon'), array_to_string(sorun, '; ')
                USING HINT = 'Hiçbir satır değişmedi. Ayrıntı: veri-olcum.sql.';
        END IF;
    END
$tasima$;

WITH g AS (UPDATE templates
           SET html_content = replace(html_content, :'eski_notrack', :'yeni_notrack')
           WHERE html_content LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 1, 'templates', 'html_content', count(*) FROM g;

WITH g AS (UPDATE templates
           SET react_email_content = replace(react_email_content, :'eski_notrack', :'yeni_notrack')
           WHERE react_email_content LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 2, 'templates', 'react_email_content', count(*) FROM g;

WITH g AS (UPDATE template_versions
           SET jsx_source = replace(jsx_source, :'eski_notrack', :'yeni_notrack')
           WHERE jsx_source LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 3, 'template_versions', 'jsx_source', count(*) FROM g;

WITH g AS (UPDATE template_versions
           SET html_source = replace(html_source, :'eski_notrack', :'yeni_notrack')
           WHERE html_source LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 4, 'template_versions', 'html_source', count(*) FROM g;

WITH g AS (UPDATE template_versions
           SET html_content = replace(html_content, :'eski_notrack', :'yeni_notrack')
           WHERE html_content LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 5, 'template_versions', 'html_content', count(*) FROM g;

-- The top-level type with jsonb_set, then the attribute in the document's
-- strings through its text.
WITH g AS (UPDATE template_versions
           SET visual_source = replace(
                   (CASE
                        WHEN visual_source ->> 'type' = :'eski_tur'
                            THEN jsonb_set(visual_source, '{type}', to_jsonb(:'yeni_tur'::TEXT))
                        ELSE visual_source
                       END)::TEXT,
                   :'eski_notrack', :'yeni_notrack')::JSONB
           WHERE visual_source::TEXT LIKE '%' || :'eski_tur' || '%'
              OR visual_source::TEXT LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 6, 'template_versions', 'visual_source', count(*) FROM g;

-- The attribute inside JSONB strings (an HTML variable, an edit's before and
-- after, the News record), through the text: the marker holds no character
-- JSON escapes, so the text stays the same JSON.
WITH g AS (UPDATE mail_approvals
           SET body_variables = replace(body_variables::TEXT, :'eski_notrack', :'yeni_notrack')::JSONB
           WHERE body_variables::TEXT LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 8, 'mail_approvals', 'body_variables', count(*) FROM g;

WITH g AS (UPDATE mail_approval_events
           SET changes = replace(changes::TEXT, :'eski_notrack', :'yeni_notrack')::JSONB
           WHERE changes::TEXT LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 9, 'mail_approval_events', 'changes', count(*) FROM g;

WITH g AS (UPDATE mail_tasks
           SET body_variables = replace(body_variables::TEXT, :'eski_notrack', :'yeni_notrack')::JSONB
           WHERE body_variables::TEXT LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 10, 'mail_tasks', 'body_variables', count(*) FROM g;

WITH g AS (UPDATE mail_tasks
           SET source_snapshot = replace(source_snapshot::TEXT, :'eski_notrack', :'yeni_notrack')::JSONB
           WHERE source_snapshot::TEXT LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 11, 'mail_tasks', 'source_snapshot', count(*) FROM g;

WITH g AS (UPDATE mail_tasks
           SET sent_by = :'yeni_gonderen'
           WHERE sent_by = :'eski_gonderen'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 12, 'mail_tasks', 'sent_by', count(*) FROM g;

WITH g AS (UPDATE mail_queue
           SET body_html = replace(body_html, :'eski_notrack', :'yeni_notrack')
           WHERE body_html LIKE '%' || :'eski_notrack' || '%'
           RETURNING 1)
INSERT INTO tasima_guncellenen SELECT 13, 'mail_queue', 'body_html', count(*) FROM g;

-- check_mail_approval now, not at COMMIT: a refusal stops here, before the
-- result is printed.
SET CONSTRAINTS ALL IMMEDIATE;

CREATE TEMP TABLE tasima_sonra ON COMMIT DROP AS SELECT * FROM tasima_sayim;

DO
$tasima$
    DECLARE
        sorun TEXT[] := '{}';
        r     RECORD;
    BEGIN
        FOR r IN
            SELECT o.tablo, o.sutun, o.kalip AS eski, o.satir AS eski_satir, o.adet AS eski_adet,
                   se.satir AS kalan_satir, se.adet AS kalan_adet, se.harf AS kalan_harf,
                   sy.kalip AS yeni, sy.satir AS yeni_satir, sy.adet AS yeni_adet
            FROM tasima_once o
                     JOIN tasima_sonra se ON se.sira = o.sira AND se.yon = 'eski'
                     JOIN tasima_sonra sy ON sy.sira = o.sira AND sy.yon = 'yeni'
            WHERE o.yon = 'eski'
            ORDER BY o.sira
            LOOP
                IF r.kalan_satir <> 0 OR r.kalan_adet <> 0 OR r.kalan_harf <> 0 THEN
                    sorun := sorun || format('%s.%s: %s satırda %s kaldı', r.tablo, r.sutun, r.kalan_satir + r.kalan_harf, r.eski);
                END IF;
                IF r.yeni_satir <> r.eski_satir OR r.yeni_adet <> r.eski_adet THEN
                    sorun := sorun || format('%s.%s: önce %s satırda %s geçiş %s, sonra %s satırda %s geçiş %s',
                                             r.tablo, r.sutun, r.eski_satir, r.eski_adet, r.eski, r.yeni_satir, r.yeni_adet, r.yeni);
                END IF;
            END LOOP;
        FOR r IN
            SELECT o.tablo, o.satir AS once, s.satir AS sonra
            FROM tasima_satir_once o
                     JOIN tasima_satir s ON s.tablo = o.tablo
            WHERE o.satir <> s.satir
            LOOP
                sorun := sorun || format('%s: satır sayısı %s idi, %s oldu', r.tablo, r.once, r.sonra);
            END LOOP;
        -- visual_source (6) moves two markers in one update.
        FOR r IN
            SELECT g.tablo, g.sutun, g.satir, o.satir AS beklenen
            FROM tasima_guncellenen g
                     JOIN tasima_once o ON o.sira = g.sira AND o.yon = 'eski'
            WHERE g.sira <> 6
              AND g.satir <> o.satir
            LOOP
                sorun := sorun || format('%s.%s: %s satır güncellendi, eski kalıp %s satırdaydı', r.tablo, r.sutun, r.satir, r.beklenen);
            END LOOP;
        IF cardinality(sorun) > 0 THEN
            RAISE EXCEPTION 'SkyNotify veri göçü (%) tutmadı, geri alındı: %', current_setting('tasima.yon'), array_to_string(sorun, '; ')
                USING HINT = 'Hiçbir satır değişmedi.';
        END IF;
    END
$tasima$;

SELECT 'kalip', tablo, sutun, kalip, satir, adet, harf FROM tasima_sonra ORDER BY sira, yon;
SELECT 'guncellenen', tablo, sutun, satir FROM tasima_guncellenen ORDER BY sira;
SELECT 'satir', tablo, satir FROM tasima_satir;
SELECT 'sonuc', current_setting('tasima.yon'), 'tamam';
