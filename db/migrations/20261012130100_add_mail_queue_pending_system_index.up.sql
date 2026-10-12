-- The dispatcher takes due pending rows one lane at a time, oldest due first
-- (ProcessSystemQueueItems, ProcessBulkQueueItems). idx_mail_queue_active_jobs
-- has no lane: with a list send's thousands of bulk rows due, every
-- system-lane turn walked them all to the heap and took none. This index and
-- idx_mail_queue_pending_bulk (the next migration) hold each lane's pending
-- rows alone, in the order the lane takes them, so a turn reads only the rows
-- it takes. The lane is in the predicate, not a column: a range over a
-- priority column would leave the rows to sort, and the planner folds
-- (priority > 0) = false into priority <= 0, which an index on the
-- expression never matches.
--
-- CONCURRENTLY: building it does not hold back the queue's writes, which a
-- plain CREATE INDEX would for as long as it reads the table. It cannot run
-- in a transaction. golang-migrate sends a file as one statement and opens
-- no transaction, but Postgres wraps a file of several statements in one, so
-- this file holds this statement alone (docs/database-migrations.md, expand).
-- For the same reason it has no lock_timeout: it waits for the transactions
-- older than it to end (a pg_dump in progress among them) and blocks none.
--
-- A build that fails (cancelled, or the process dies) leaves an INVALID index
-- of this name and the version dirty. There is no IF NOT EXISTS, so that index
-- is never taken for a built one: drop it, then put the version back
-- (docs/database-migrations.md, dirty version).
CREATE INDEX CONCURRENTLY idx_mail_queue_pending_system
    ON mail_queue (next_attempt_at, created_at)
    WHERE status = 'pending' AND priority > 0;
