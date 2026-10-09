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
ALTER TABLE mail_queue
    ADD COLUMN claimed_at TIMESTAMPTZ,
    ADD COLUMN claimed_by TEXT;
