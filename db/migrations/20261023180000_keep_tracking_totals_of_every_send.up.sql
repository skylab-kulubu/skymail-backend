-- Every send keeps its open and click counts without people once its
-- recipients' openings and clicks go (KVKK notice s6, 2026-10-23; hub ticket
-- kvkk-s6-yayin-oncesi 13): 90 days after each mail went out the events are
-- deleted, and nothing ties an opening or a click to a person any more. A
-- send with at least 10 tracked recipients keeps the counts for good; one
-- with fewer keeps them a year from its creation, as its content, and the
-- retention run then deletes them. The reports show the counts of fewer than
-- 10 only to who may read the send's people (#97 T6).
--
-- Dropping a CHECK rewrites and reads nothing; the ACCESS EXCLUSIVE lock is
-- held for an instant. An image from before writes only sends of 10 or more,
-- as before (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

ALTER TABLE mail_task_tracking_totals
    DROP CONSTRAINT IF EXISTS mail_task_tracking_totals_anonymous;

COMMENT ON TABLE mail_task_tracking_totals IS
    'A send''s tracking counts without people, kept after its events go: for good with at least 10 tracked recipients, a year from the send with fewer.';

RESET lock_timeout;
