-- A person's tracked rows by their Keycloak subject: what a withdrawal from
-- core closes (PUT /internal/v1/tracking-withdrawals/{sub}), every mail
-- already sent to them included. Only the tracked rows are in it.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE INDEX CONCURRENTLY idx_mail_queue_tracking_subject
    ON mail_queue (recipient_sub)
    WHERE tracking_token IS NOT NULL;
