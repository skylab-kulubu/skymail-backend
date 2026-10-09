-- A row a dispatcher takes is leased to it: which process took it
-- (claimed_by, the host name and a random suffix the process picks at
-- startup) and when (claimed_at). A process puts a processing row back to
-- pending only once its lease has run out, not every processing row it finds
-- at startup: under a start-first deploy the old task is still sending the
-- rows it took when the new one starts. Only the process that took a row may
-- record how its send went.
--
-- Both are nullable and have no default, so adding them rewrites nothing, and
-- an image built before them, which names its columns, reads and writes the
-- queue as before. A processing row without claimed_at was taken by such an
-- image (or comes from a dump made before this migration); the mailer stamps
-- it the first time it sees it and lets the lease run from there.
--
-- ADD COLUMN takes mail_queue's ACCESS EXCLUSIVE lock for an instant, but it
-- waits behind any transaction holding the table, and every queue query waits
-- behind it. lock_timeout gives up after 5 seconds instead: the migration
-- fails, rolled back, the new task does not start and the old one keeps
-- sending. golang-migrate leaves the version dirty then; recovery is in
-- docs/database-migrations.md. RESET puts the session's setting back.
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN claimed_at TIMESTAMPTZ,
    ADD COLUMN claimed_by TEXT;

RESET lock_timeout;
