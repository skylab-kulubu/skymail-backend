-- The retention of recipients without an account (ADR-0062; hub sector audit
-- 2026-11-24, 06 fix 3; README, "Hesapsız alıcıların saklaması"). The
-- recipient retention run (SKYMAIL_RECIPIENT_RETENTION) deletes the people
-- without an account of a manual list a year after it was archived, and
-- empties the address and name a send, a Mail onayı request or their Forms
-- snapshot keeps of someone without an account once the send's content is
-- gone (MAIL_CONTENT_RETENTION_DAYS).
--
-- recipient_retention_runs: one row per run, counts and times only; no
-- person, address or name is ever in it. The schedule reads the last run from
-- here, so a restart or a release never runs it twice in a day, and a dry
-- run's counts can be read back before apply is turned on.
--
-- mail_tasks.recipient_retention_at and mail_approvals.recipient_retention_at:
-- when the run decided every person of the send, or of the request: emptied,
-- or found to have an account. It is not looked at again (a request is again
-- once it changes: updated_at after it). Nothing else reads them.
--
-- mail_approval_recipients.cleared_at and mail_form_source_recipients.cleared_at:
-- when the run emptied the row. An emptied row keeps its position (task_ids[i]
-- is still the send to recipients[i]; a Forms snapshot keeps its count) with
-- an empty address and name, so the address CHECK and the once-per-request
-- (once-per-snapshot) index leave emptied rows out.
--
-- account_at on mail_queue (recipient_account_at), mail_approval_recipients,
-- mail_form_source_recipients and recipients: when the run found the row's
-- address to be someone's account. The row is kept, and the run does not ask
-- about it again: a run asks Keycloak about a bounded number of addresses, and
-- without the answer kept the same accounts would fill every run. Nothing
-- else reads them; a recipients row whose address changes forgets it.
--
-- Expand only (docs/database-migrations.md): a new table, nullable columns,
-- CHECKs made looser, indexes made partial and a trigger that empties a
-- column only this image reads. An image from before never
-- writes cleared_at and writes only non-empty addresses, as before; it reads
-- an emptied row as an empty address and name.
SET lock_timeout = '5s';

CREATE TABLE recipient_retention_runs
(
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    mode        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'running',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    counts      JSONB       NOT NULL DEFAULT '{}',
    CONSTRAINT recipient_retention_runs_mode CHECK (mode IN ('dry-run', 'apply')),
    CONSTRAINT recipient_retention_runs_status CHECK (status IN ('running', 'ok', 'partial', 'refused_large', 'would_refuse_large', 'failed')),
    CONSTRAINT recipient_retention_runs_finished CHECK ((status = 'running') = (finished_at IS NULL))
);

CREATE INDEX idx_recipient_retention_runs_mode ON recipient_retention_runs (mode, started_at);

COMMENT ON TABLE recipient_retention_runs IS
    'Receipts of the recipient retention run: mode, times, status, counts. No person, address or name.';

ALTER TABLE mail_tasks
    ADD COLUMN recipient_retention_at TIMESTAMPTZ;

COMMENT ON COLUMN mail_tasks.recipient_retention_at IS
    'When the recipient retention run decided every recipient of the send whose content is gone: address and name emptied, or an account found. Null until then.';

ALTER TABLE mail_approvals
    ADD COLUMN recipient_retention_at TIMESTAMPTZ;

COMMENT ON COLUMN mail_approvals.recipient_retention_at IS
    'When the recipient retention run decided every person of the request: address and name emptied, or an account found. Looked at again once updated_at is later.';

ALTER TABLE mail_approval_recipients
    ADD COLUMN cleared_at TIMESTAMPTZ,
    ADD COLUMN account_at TIMESTAMPTZ;

COMMENT ON COLUMN mail_approval_recipients.cleared_at IS
    'When the recipient retention run emptied the address and name of this person without an account; null otherwise.';
COMMENT ON COLUMN mail_approval_recipients.account_at IS
    'When the recipient retention run found the address to be someone''s account: kept, not asked about again.';

ALTER TABLE mail_queue
    ADD COLUMN recipient_account_at TIMESTAMPTZ;

COMMENT ON COLUMN mail_queue.recipient_account_at IS
    'When the recipient retention run found the row''s address, with no subject, to be someone''s account: kept, not asked about again.';

ALTER TABLE recipients
    ADD COLUMN account_at TIMESTAMPTZ;

COMMENT ON COLUMN recipients.account_at IS
    'When the recipient retention run found the address to be someone''s account: their memberships of a list archived a year ago stay. Null again once the address changes.';

ALTER TABLE mail_form_source_recipients
    ADD COLUMN cleared_at TIMESTAMPTZ,
    ADD COLUMN account_at TIMESTAMPTZ;

COMMENT ON COLUMN mail_form_source_recipients.cleared_at IS
    'When the recipient retention run emptied the address and name of this respondent without an account; null otherwise.';
COMMENT ON COLUMN mail_form_source_recipients.account_at IS
    'When the recipient retention run found the address to be someone''s account: kept, not asked about again.';

ALTER TABLE mail_approval_recipients
    DROP CONSTRAINT mail_approval_recipients_email;
ALTER TABLE mail_approval_recipients
    ADD CONSTRAINT mail_approval_recipients_email CHECK (btrim(email) <> '' OR cleared_at IS NOT NULL) NOT VALID;
ALTER TABLE mail_approval_recipients
    VALIDATE CONSTRAINT mail_approval_recipients_email;

DROP INDEX mail_approval_recipients_email_once;
CREATE UNIQUE INDEX mail_approval_recipients_email_once ON mail_approval_recipients (approval_id, lower(email))
    WHERE cleared_at IS NULL;

ALTER TABLE mail_form_source_recipients
    DROP CONSTRAINT mail_form_source_recipients_email_check;
ALTER TABLE mail_form_source_recipients
    ADD CONSTRAINT mail_form_source_recipients_email_check
        CHECK ((btrim(email) <> '' OR cleared_at IS NOT NULL) AND char_length(email) <= 254) NOT VALID;
ALTER TABLE mail_form_source_recipients
    VALIDATE CONSTRAINT mail_form_source_recipients_email_check;

DROP INDEX mail_form_source_recipients_email_once;
CREATE UNIQUE INDEX mail_form_source_recipients_email_once ON mail_form_source_recipients (source_id, lower(email))
    WHERE cleared_at IS NULL;

-- A recipients row whose address changes forgets what the run found.
CREATE FUNCTION recipients_forget_account() RETURNS trigger
    LANGUAGE plpgsql SET search_path = pg_catalog, public AS
$$
BEGIN
    IF lower(btrim(NEW.email)) IS DISTINCT FROM lower(btrim(OLD.email)) THEN
        NEW.account_at := NULL;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER recipients_forget_account BEFORE UPDATE OF email ON recipients
    FOR EACH ROW EXECUTE FUNCTION recipients_forget_account();

RESET lock_timeout;
