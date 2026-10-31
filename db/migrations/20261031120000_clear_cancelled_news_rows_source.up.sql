-- A cancelled row of a News send gives up its source_ref (hub: Yusuf's
-- decision of 2026-10-31; review duyuru-haber B3): its recipient never got
-- the record's mail, so a later send of the record goes to them. From this
-- image on CancelPendingRows and CancelClaimedRows write it so; this clears
-- the rows an image from before (20261027120000) cancelled with it.
--
-- Expand only: an image from before never reads source_ref of a failed row
-- but in ListNewsSourceAddresses, where leaving it out is the point. Few
-- rows: only cancelled rows of News sends, found through
-- idx_mail_queue_news_once.
SET lock_timeout = '5s';

UPDATE mail_queue
SET source_ref = NULL
WHERE source_ref IS NOT NULL
  AND status = 'failed'
  AND error = 'cancelled';

RESET lock_timeout;
