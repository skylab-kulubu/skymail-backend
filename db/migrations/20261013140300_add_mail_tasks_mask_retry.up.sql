-- When a failed masking of a send is tried again, and the failures not yet
-- counted (#63 review, Ek 3, Z1). A send whose masking fails is put back:
-- content_mask_retry_at, later each time (doubling, a day at most), so the
-- sends that never failed go first and a few broken ones at the head of the
-- queue cannot hold the others back. Its failure is counted
-- (content_mask_failures, emptied at 3) only once a run has masked some
-- other send, which shows the fault is the send's and not SkyMail's; until
-- then it waits in content_mask_unconfirmed.
--
-- Both columns are new with constant defaults: no rewrite, and an image from
-- before them neither reads nor writes them. lock_timeout as in
-- 20261013140000.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN content_mask_retry_at   TIMESTAMPTZ,
    ADD COLUMN content_mask_unconfirmed SMALLINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN mail_tasks.content_mask_retry_at IS
    'When the content sweep tries masking the send again after a failure; null for a send that has not failed.';
COMMENT ON COLUMN mail_tasks.content_mask_unconfirmed IS
    'Failed maskings not counted yet: counted into content_mask_failures once a run masks another send.';

RESET lock_timeout;
