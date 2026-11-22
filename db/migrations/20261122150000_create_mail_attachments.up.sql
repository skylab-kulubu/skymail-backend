-- A Mail attachment (ADR 0082 decision 6, CONTEXT "Mail attachment"; hub
-- ticket bilet-qr-ve-pdf 08): a file a service hands with one mail, today
-- core's ticket PDF. Only a service's single-recipient mail
-- (POST /v1/mail_tasks/single, POST /v1/notifications to one person), only on
-- the e-mail channel, PDF or PNG, at most two and 512 KB in all; the handler
-- checks that before anything is written.
--
-- One row per file of one queue row. content holds the file only until the
-- row's outcome is written: the trigger below empties it in the transaction
-- that writes sent or failed (sent, failed for good, failed at queueing,
-- cancelled, closed after a restore: every path, the old image's during a
-- start-first deploy too). What stays is what the send's report shows: the
-- name, the type and the size, never the file. The row goes with its queue
-- row (Account erasure deletes a pending row to the person, ON DELETE
-- CASCADE), on Account erasure of mail to the person, and once the content
-- sweep has emptied its send.
--
-- A new table, a function and a trigger only: an image from before neither
-- reads nor writes the table, and the trigger only empties what it never
-- wrote (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_attachments
(
    queue_id     UUID        NOT NULL REFERENCES mail_queue (id) ON DELETE CASCADE,
    position     SMALLINT    NOT NULL,
    filename     TEXT        NOT NULL,
    content_type TEXT        NOT NULL,
    size_bytes   INTEGER     NOT NULL,
    content      BYTEA,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (queue_id, position),
    CONSTRAINT mail_attachments_position CHECK (position BETWEEN 1 AND 10),
    CONSTRAINT mail_attachments_filename CHECK (filename ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$'),
    CONSTRAINT mail_attachments_content_type CHECK (content_type ~ '^[a-z]+/[a-z0-9.+-]+$'),
    CONSTRAINT mail_attachments_size CHECK (size_bytes > 0),
    CONSTRAINT mail_attachments_content_size CHECK (content IS NULL OR octet_length(content) = size_bytes)
);

COMMENT ON TABLE mail_attachments IS
    'A service''s Mail attachment on one queue row (ADR 0082): the file until the row is sent or fails for good, then only its name, type and size.';
COMMENT ON COLUMN mail_attachments.content IS
    'The file, kept only while its queue row is still to go out; emptied by mail_queue_drop_attachment_content when the row''s outcome is written.';

CREATE FUNCTION mail_queue_drop_attachment_content() RETURNS TRIGGER
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
AS
$$
BEGIN
    UPDATE mail_attachments
    SET content = NULL
    WHERE queue_id = NEW.id
      AND content IS NOT NULL;
    RETURN NULL;
END
$$;

CREATE TRIGGER mail_queue_drop_attachment_content
    AFTER UPDATE OF status
    ON mail_queue
    FOR EACH ROW
    WHEN (NEW.status IN ('sent', 'failed') AND OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION mail_queue_drop_attachment_content();

RESET lock_timeout;
