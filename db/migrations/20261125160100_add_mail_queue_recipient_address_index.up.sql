-- A person's mail by their address, newest first: what GET
-- /v1/recipients/history reads (lower(recipient_email) = lower(address),
-- ORDER BY created_at DESC, id DESC), and what account erasure finds by the
-- person's addresses. lower() as both of them compare it.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE INDEX CONCURRENTLY idx_mail_queue_recipient_address
    ON mail_queue (lower(recipient_email), created_at DESC, id DESC);
