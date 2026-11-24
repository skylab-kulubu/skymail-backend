-- Back to before the recipient retention run. What it emptied or deleted
-- does not come back, and could not. An emptied Mail onayı or Forms row takes
-- a stand-in address (unique within its request or snapshot, in a reserved
-- domain) so the address CHECKs and the once-only indexes hold again.
SET lock_timeout = '5s';

UPDATE mail_approval_recipients
SET email = 'saklama-suresi-doldu-' || position || '@invalid'
WHERE cleared_at IS NOT NULL;
UPDATE mail_form_source_recipients
SET email = 'saklama-suresi-doldu-' || position || '@invalid'
WHERE cleared_at IS NOT NULL;
-- The updates queued check_mail_approval's and check_mail_form_source's
-- deferred events; they run now, or the indexes below cannot be made in this
-- transaction.
SET CONSTRAINTS ALL IMMEDIATE;

DROP TRIGGER recipients_forget_account ON recipients;
DROP FUNCTION recipients_forget_account();

DROP INDEX mail_form_source_recipients_email_once;
CREATE UNIQUE INDEX mail_form_source_recipients_email_once ON mail_form_source_recipients (source_id, lower(email));

ALTER TABLE mail_form_source_recipients
    DROP CONSTRAINT mail_form_source_recipients_email_check;
ALTER TABLE mail_form_source_recipients
    ADD CONSTRAINT mail_form_source_recipients_email_check CHECK (btrim(email) <> '' AND char_length(email) <= 254);

ALTER TABLE mail_form_source_recipients
    DROP COLUMN cleared_at,
    DROP COLUMN account_at;
ALTER TABLE recipients
    DROP COLUMN account_at;
ALTER TABLE mail_queue
    DROP COLUMN recipient_account_at;

DROP INDEX mail_approval_recipients_email_once;
CREATE UNIQUE INDEX mail_approval_recipients_email_once ON mail_approval_recipients (approval_id, lower(email));

ALTER TABLE mail_approval_recipients
    DROP CONSTRAINT mail_approval_recipients_email;
ALTER TABLE mail_approval_recipients
    ADD CONSTRAINT mail_approval_recipients_email CHECK (btrim(email) <> '');

ALTER TABLE mail_approval_recipients
    DROP COLUMN cleared_at,
    DROP COLUMN account_at;
ALTER TABLE mail_approvals
    DROP COLUMN recipient_retention_at;
ALTER TABLE mail_tasks
    DROP COLUMN recipient_retention_at;

DROP TABLE recipient_retention_runs;

RESET lock_timeout;
