-- The bulk lane's pending rows, in the order the lane takes them: the other
-- half of idx_mail_queue_pending_system (the previous migration, which says
-- why). One statement, CONCURRENTLY, outside a transaction, and the same
-- recovery from a failed build: drop the INVALID index, put the version back.
CREATE INDEX CONCURRENTLY idx_mail_queue_pending_bulk
    ON mail_queue (next_attempt_at, created_at)
    WHERE status = 'pending' AND priority <= 0;
