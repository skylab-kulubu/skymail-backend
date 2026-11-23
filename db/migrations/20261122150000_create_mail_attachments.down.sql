-- The attachments go with the table: a mail still queued goes out without
-- its file, as an image from before sends it. The sends and their rows stay.
SET lock_timeout = '5s';

DROP TRIGGER IF EXISTS mail_queue_drop_attachment_content ON mail_queue;
DROP FUNCTION IF EXISTS mail_queue_drop_attachment_content();
DROP TABLE IF EXISTS mail_attachments;

RESET lock_timeout;
