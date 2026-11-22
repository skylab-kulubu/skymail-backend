-- The keys go; the sends they queued stay. An image from before reads no key:
-- a retry then queues the mail again, as it did before.
SET lock_timeout = '5s';

DROP TABLE IF EXISTS mail_task_idempotency_keys;

RESET lock_timeout;
