-- Back to before the recipient retention run. What it emptied or deleted
-- does not come back, and could not. An emptied Mail onayı row takes a
-- stand-in address (unique within its request, in a reserved domain) so the
-- address CHECK and the once-per-request index hold again.
SET lock_timeout = '5s';

UPDATE mail_approval_recipients
SET email = 'saklama-suresi-doldu-' || position || '@invalid'
WHERE cleared_at IS NOT NULL;

DROP INDEX mail_approval_recipients_email_once;
CREATE UNIQUE INDEX mail_approval_recipients_email_once ON mail_approval_recipients (approval_id, lower(email));

ALTER TABLE mail_approval_recipients
    DROP CONSTRAINT mail_approval_recipients_email;
ALTER TABLE mail_approval_recipients
    ADD CONSTRAINT mail_approval_recipients_email CHECK (btrim(email) <> '');

ALTER TABLE mail_approval_recipients
    DROP COLUMN cleared_at;
ALTER TABLE mail_approvals
    DROP COLUMN recipient_retention_at;
ALTER TABLE mail_tasks
    DROP COLUMN recipient_retention_at;

DROP TABLE recipient_retention_runs;

RESET lock_timeout;
