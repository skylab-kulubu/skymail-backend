-- SkyNotify ad taşıması (ADR 0091, hub bileti skynotify-ad 02): veri ölçümü.
--
-- Salt okur. Satırlarda saklı üç eski ad kimliğini ve yeni karşılıklarını
-- sayar; veri-ileri.sql / veri-geri.sql'in taşıdığı her sütun için, her adımda
-- aynı biçimde. Pencerenin Ö8 ölçümü ve provası, S4b/P2 karşılaştırması ve
-- doğrulaması bunu koşar:
--
--   docker exec -i <konteyner> psql -X -v ON_ERROR_STOP=1 -1 -U "$POSTGRES_USER" -d skynotify -f - < veri-olcum.sql
--
-- (-1 tek anlık görüntü verir: bütün sayılar aynı anın.) Kimlikler:
--
--   tur       Visual belge türü, template_versions.visual_source'un üst
--             düzey "type" alanı;
--   notrack   bağlantıyı takipsiz bırakan HTML özniteliği: şablonlarda,
--             sürümlerde ve taslaklarda, onay isteklerinde ve olaylarında,
--             gönderimlerin değişkenlerinde ve haber anlık kopyasında, kuyruk
--             gövdelerinde;
--   gonderen  SkyNotify'ın kendi gönderdiği postanın mail_tasks.sent_by değeri.
--
-- Çıktı, "|" ile ayrılmış satırlar:
--
--   kalip|tablo|sütun|kalıp|satır|adet|harf
--       taşınan her sütun ve her kalıp (eski ve yeni) için, sıfır olsa da:
--       kalıbı içeren satır, toplam geçiş, kalıbı başka harf büyüklüğüyle
--       içeren satır (harf > 0 taşımayı durdurur);
--   disarida|tablo|sütun|kalıp|satır
--       taşınmayan bir metin/JSON sütununda bir kalıp (yalnız sıfır değilse;
--       eski kalıp varsa veri-ileri.sql durur);
--   eski_ad|tablo|sütun|skymail|satır
--       herhangi bir metin/JSON sütununda eski ad, harf büyüklüğüne bakmadan
--       (bilgi: örn. eski seed'in şablon kaynağına yazdığı yorum);
--   yol|tablo|sütun|kalıp|JSON yolu|adet
--       JSONB sütunlarında kalıbın geçtiği dizge yolları ($.a.b, dizi [*]);
--   satir|tablo|satır sayısı
--   boyut|tablo ya da veritabani|bayt|tahmini satır
--   risk|mail_queue_erase_after_send|n
--       eski kalıplı, gönderilmiş ya da düşmüş ve erase_after_send bir
--       gönderimin satırı: tetikleyici bu satırı silerdi, veri-ileri.sql durur;
--   bilgi|onay_guncellenecek|n
--       eski kalıplı onay isteği (check_mail_approval her biri için koşar);
--   bilgi|kuyruk_bekleyen_cikis_baglantili|n
--       gönderilmemiş, abonelikten çıkış bağlantılı kuyruk satırı (Ö7: 0 olmalı);
--   sonuc|olcum|tamam
\set ON_ERROR_STOP on
\set QUIET on
\pset format unaligned
\pset tuples_only on
\pset fieldsep '|'
\pset footer off
\pset pager off

\set eski_tur 'skymail.visual'
\set yeni_tur 'skynotify.visual'
\set eski_notrack 'data-skymail-notrack'
\set yeni_notrack 'data-skynotify-notrack'
\set eski_gonderen 'skymail'
\set yeni_gonderen 'skynotify'

SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY;

SELECT 'kalip', s.tablo, s.sutun, s.kalip, s.satir, s.adet, s.harf
FROM (
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
) s
ORDER BY s.sira, s.yon;

-- Every text and JSON column of every table, one scan a table: a marker in a
-- column the files do not move, and the old name anywhere.
WITH kalip(aile, metin) AS (
    VALUES ('tur', :'eski_tur'),
           ('tur', :'yeni_tur'),
           ('notrack', :'eski_notrack'),
           ('notrack', :'yeni_notrack'),
           ('gonderen', :'eski_gonderen'),
           ('gonderen', :'yeni_gonderen')),
     kapsam(tablo, sutun, aile) AS (
         VALUES ('templates', 'html_content', 'notrack'),
                ('templates', 'react_email_content', 'notrack'),
                ('template_versions', 'jsx_source', 'notrack'),
                ('template_versions', 'html_source', 'notrack'),
                ('template_versions', 'html_content', 'notrack'),
                ('template_versions', 'visual_source', 'tur'),
                ('template_versions', 'visual_source', 'notrack'),
                ('mail_approvals', 'body_variables', 'notrack'),
                ('mail_approval_events', 'changes', 'notrack'),
                ('mail_tasks', 'body_variables', 'notrack'),
                ('mail_tasks', 'source_snapshot', 'notrack'),
                ('mail_tasks', 'sent_by', 'gonderen'),
                ('mail_queue', 'body_html', 'notrack')),
     sutun(tablo, sutun, sira) AS (
         SELECT c.table_name::text, c.column_name::text, c.ordinal_position
         FROM information_schema.columns c
                  JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
         WHERE c.table_schema = 'public'
           AND t.table_type = 'BASE TABLE'
           AND c.table_name <> 'schema_migrations'
           AND (c.data_type IN ('text', 'character varying', 'character', 'json', 'jsonb')
             OR c.udt_name IN ('_text', '_varchar'))),
     denetim(tablo, sira, sutun, tur, metin, kosul) AS (
         SELECT s.tablo, s.sira, s.sutun, 'disarida', p.metin,
                CASE p.aile
                    WHEN 'gonderen' THEN format('%I::text = %L', s.sutun, p.metin)
                    ELSE format('strpos(%I::text, %L) > 0', s.sutun, p.metin)
                    END
         FROM sutun s
                  CROSS JOIN kalip p
         WHERE NOT EXISTS (SELECT 1 FROM kapsam k WHERE k.tablo = s.tablo AND k.sutun = s.sutun AND k.aile = p.aile)
         UNION ALL
         SELECT s.tablo, s.sira, s.sutun, 'eski_ad', :'eski_gonderen',
                format('%I::text ILIKE %L', s.sutun, '%' || :'eski_gonderen' || '%')
         FROM sutun s),
     numarali AS (SELECT d.*, row_number() OVER (PARTITION BY d.tablo ORDER BY d.sira, d.tur, d.metin) AS n
                  FROM denetim d)
SELECT format('SELECT x.tur, %L, x.sutun, x.metin, x.satir FROM (SELECT %s FROM %I) s'
                  || ' CROSS JOIN LATERAL (VALUES %s) AS x(tur, sutun, metin, satir) WHERE x.satir > 0',
              n.tablo,
              string_agg(format('count(*) FILTER (WHERE %s) AS s%s', n.kosul, n.n), ', ' ORDER BY n.n),
              n.tablo,
              string_agg(format('(%L, %L, %L, s.s%s)', n.tur, n.sutun, n.metin, n.n), ', ' ORDER BY n.n))
FROM numarali n
GROUP BY n.tablo
ORDER BY n.tablo
\gexec

-- Where in a JSONB column a marker is: each string holding it, by path.
WITH RECURSIVE kalip(aile, metin) AS (
    VALUES ('tur', :'eski_tur'),
           ('tur', :'yeni_tur'),
           ('notrack', :'eski_notrack'),
           ('notrack', :'yeni_notrack')),
               kaynak(tablo, sutun, aile, belge) AS (
                   SELECT 'template_versions', 'visual_source', a.aile, visual_source
                   FROM template_versions
                            CROSS JOIN (VALUES ('tur'), ('notrack')) a(aile)
                   UNION ALL SELECT 'mail_approvals', 'body_variables', 'notrack', body_variables FROM mail_approvals
                   UNION ALL SELECT 'mail_approval_events', 'changes', 'notrack', changes FROM mail_approval_events
                   UNION ALL SELECT 'mail_tasks', 'body_variables', 'notrack', body_variables FROM mail_tasks
                   UNION ALL SELECT 'mail_tasks', 'source_snapshot', 'notrack', source_snapshot FROM mail_tasks),
               yuru(tablo, sutun, metin, yol, belge) AS (
                   SELECT k.tablo, k.sutun, p.metin, '$', k.belge
                   FROM kaynak k
                            JOIN kalip p ON p.aile = k.aile
                   WHERE strpos(k.belge::text, p.metin) > 0
                   UNION ALL
                   SELECT y.tablo, y.sutun, y.metin, y.yol || e.parca, e.deger
                   FROM yuru y
                            CROSS JOIN LATERAL (SELECT '.' || o.key, o.value
                                                FROM jsonb_each(CASE WHEN jsonb_typeof(y.belge) = 'object' THEN y.belge END) o
                                                UNION ALL
                                                SELECT '[*]', a.value
                                                FROM jsonb_array_elements(CASE WHEN jsonb_typeof(y.belge) = 'array' THEN y.belge END) a)
                       AS e(parca, deger)
                   WHERE strpos(e.deger::text, y.metin) > 0)
SELECT 'yol', tablo, sutun, metin, yol, count(*)
FROM yuru
WHERE jsonb_typeof(belge) = 'string'
GROUP BY tablo, sutun, metin, yol
ORDER BY tablo, sutun, metin, yol;

SELECT 'satir', 'templates', count(*) FROM templates
UNION ALL SELECT 'satir', 'template_versions', count(*) FROM template_versions
UNION ALL SELECT 'satir', 'mail_approvals', count(*) FROM mail_approvals
UNION ALL SELECT 'satir', 'mail_approval_events', count(*) FROM mail_approval_events
UNION ALL SELECT 'satir', 'mail_tasks', count(*) FROM mail_tasks
UNION ALL SELECT 'satir', 'mail_queue', count(*) FROM mail_queue;

SELECT 'boyut', 'veritabani', pg_database_size(current_database()), NULL::BIGINT
UNION ALL
SELECT 'boyut', c.relname::text, pg_total_relation_size(c.oid), c.reltuples::BIGINT
FROM pg_class c
         JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relname IN ('templates', 'template_versions', 'mail_approvals', 'mail_approval_events', 'mail_tasks', 'mail_queue');

SELECT 'risk', 'mail_queue_erase_after_send', count(*)
FROM mail_queue q
         JOIN mail_tasks t ON t.id = q.task_id
WHERE t.erase_after_send
  AND q.status IN ('sent', 'failed')
  AND q.body_html LIKE '%' || :'eski_notrack' || '%';

SELECT 'bilgi', 'onay_guncellenecek', count(*)
FROM mail_approvals
WHERE body_variables::text LIKE '%' || :'eski_notrack' || '%';

SELECT 'bilgi', 'kuyruk_bekleyen_cikis_baglantili', count(*)
FROM mail_queue
WHERE status IS DISTINCT FROM 'sent'
  AND status IS DISTINCT FROM 'failed'
  AND list_unsubscribe_url IS NOT NULL;

SELECT 'sonuc', 'olcum', 'tamam';
