-- A send made from a published News record (POST /v1/news_sends; hub ticket
-- duyuru-haber 06, SPEC §5.7 G5; README, "Haberden gönderim").
--
-- mail_tasks:
--   source_kind: 'news' for such a send; NULL for every other.
--   source_ref: news:<slug>, the record's canonical slug as inscribed answers
--     it. The key of "bir kez": a record's mail goes to a mailbox once.
--   source_version: the record's version when the send was made; the report
--     says "vN gönderildi" against the record's version now.
--   source_snapshot: what the mail was made of, as inscribed answered it then
--     (title, summary, body, heroImage, heroAlt, cta, kind, audience, teams,
--     updatedAt): a later edit changes the web, never the mail or the report.
--     No recipient is in it.
--   source_audience: the audience asked for, canonical (members, or
--     teams:<path>,<path> sorted): the same record to the same audience is
--     one send (the next migrations' unique index).
--   source_counts: core's counts and skips for the audience (counts,
--     skipped) and the recipients left out because the record's mail had
--     already gone to them (already_received). Numbers only.
--
-- mail_queue.source_ref: the send's source_ref on each row queued to go; a
-- row written failed at queueing (a suppressed address, a mail that did not
-- render) has none, so "eksik kalanlara gönder" asks again for it. The
-- unique index on it is its own migration (CONCURRENTLY).
--
-- Expand only (docs/database-migrations.md): nullable columns without a
-- default rewrite nothing, and an image from before names its columns (sqlc
-- spells out *), so it reads and writes both tables as before and leaves
-- them NULL, which the checks allow. The checks are NOT VALID: they hold for
-- every row written from now on without reading the tables under the ACCESS
-- EXCLUSIVE lock, and every row already there is NULL.
--
-- lock_timeout as in 20261014150000: the locks are held for an instant, but
-- wait behind any transaction holding the tables; past 5 seconds the
-- migration fails, rolled back, and the old task keeps sending.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN source_kind     TEXT,
    ADD COLUMN source_ref      TEXT,
    ADD COLUMN source_version  INTEGER,
    ADD COLUMN source_snapshot JSONB,
    ADD COLUMN source_audience TEXT,
    ADD COLUMN source_counts   JSONB,
    ADD CONSTRAINT mail_tasks_source_check CHECK (
        (source_kind IS NULL AND source_ref IS NULL AND source_version IS NULL AND source_snapshot IS NULL
            AND source_audience IS NULL AND source_counts IS NULL)
            OR (source_kind = 'news' AND source_ref ~ '^news:.{1,200}$' AND source_version IS NOT NULL
            AND jsonb_typeof(source_snapshot) = 'object' AND source_audience IS NOT NULL)
        ) NOT VALID;

ALTER TABLE mail_queue
    ADD COLUMN source_ref TEXT,
    ADD CONSTRAINT mail_queue_source_ref_check CHECK (source_ref IS NULL OR source_ref ~ '^news:.{1,200}$') NOT VALID;

COMMENT ON COLUMN mail_tasks.source_snapshot IS
    'The News record the send was made of, as inscribed answered it then; no recipient.';
COMMENT ON COLUMN mail_queue.source_ref IS
    'news:<slug> on a row of a News send queued to go: the key of the record''s once-per-mailbox rule.';

RESET lock_timeout;
