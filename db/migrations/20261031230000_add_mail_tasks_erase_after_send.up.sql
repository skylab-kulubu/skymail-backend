-- A send that keeps no record of whom it went to (core's account deletion
-- notices, ADR-0051; hub notes/hesap-silme-postalari-20261031.md). Core sends
-- the person "your deletion request was received" and "your account was
-- deleted" after SkyMail has already run the person's Erasure command, so
-- nothing would ever erase these rows. A row of such a send is erased by
-- itself the moment its outcome is written: what Account erasure leaves of
-- mail to an erased person (internal/database/account_erasure.go, "mail to
-- the person") and nothing more. The row, its status and its dates stay, so
-- the send's counts do; the address, the name, the rendered mail and the
-- send's variables go. An error that is codes only stays: a relay's or
-- Graph's refusal (the stage and the reply's codes, never its text; mailer
-- smtpReply, graphReply) and the fixed reasons mail_task_status() tells a
-- skipped row by (reserved_domain:, suppressed:, preference:, cancelled). Any
-- other error, which could quote the address (docs/data-lifecycle.md), goes.
--
-- A trigger rather than the mailer's own statements: every path that writes a
-- row's outcome (sent, failed for good, failed at queueing for a reserved or
-- suppressed address or a render error, cancelled, closed after a restore)
-- goes through it, and an image from before this one, which still runs during
-- a start-first deploy, never sets the flag.
--
-- Expand only: a NOT NULL column with a constant default is a catalog change,
-- and an image from before neither reads nor writes it.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN erase_after_send BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN mail_tasks.erase_after_send IS
    'A send that keeps no record of its recipient (POST /mail_tasks/single erase_after_send, a service''s only): each row is erased when its outcome is written, and the send''s variables with it.';

CREATE FUNCTION mail_queue_erase_after_send() RETURNS TRIGGER
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
AS
$$
BEGIN
    IF EXISTS (SELECT 1 FROM mail_tasks t WHERE t.id = NEW.task_id AND t.erase_after_send) THEN
        NEW.recipient_full_name := 'Silinmiş kullanıcı';
        NEW.recipient_email := '';
        NEW.subject := '';
        NEW.body := '';
        NEW.body_html := NULL;
        NEW.recipient_sub := NULL;
        NEW.list_unsubscribe_url := NULL;
        NEW.tracking_token := NULL;
        IF NEW.error IS NOT NULL
            AND NEW.error <> 'cancelled'
            AND NEW.error !~ '^(smtp_rejected|smtp_deferred|graph_rejected|graph_deferred|reserved_domain|suppressed|preference): ' THEN
            NEW.error := NULL;
        END IF;
        UPDATE mail_tasks
        SET body_variables = '{}'::jsonb
        WHERE id = NEW.task_id
          AND body_variables <> '{}'::jsonb;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER mail_queue_erase_after_send
    BEFORE INSERT OR UPDATE
    ON mail_queue
    FOR EACH ROW
    WHEN (NEW.status IN ('sent', 'failed'))
EXECUTE FUNCTION mail_queue_erase_after_send();

RESET lock_timeout;
