-- Back to counts of sends with at least 10 tracked recipients only. The
-- counts of smaller sends go first: an image from before never deletes them.
SET lock_timeout = '5s';

DELETE FROM mail_task_tracking_totals WHERE tracked_recipients < 10;

ALTER TABLE mail_task_tracking_totals
    ADD CONSTRAINT mail_task_tracking_totals_anonymous CHECK (tracked_recipients >= 10);

COMMENT ON TABLE mail_task_tracking_totals IS
    'A send''s tracking counts without people, kept after its events go; only sends with at least 10 tracked recipients.';

RESET lock_timeout;
