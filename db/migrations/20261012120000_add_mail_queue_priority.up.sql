-- A queue row's lane: which mail the dispatcher takes first. 1 is the system
-- lane, the single mail a person is waiting for (a login code, a certificate,
-- a Forms confirmation; POST /mail_tasks/single); 0 is the bulk lane, a send
-- to a mailing list or a Keycloak group, or an approved send. A dispatcher
-- takes system rows first and leaves bulk rows at most part of its workers,
-- so a bulk send never holds a system mail back.
--
-- The default is the system lane: every row queued before this migration, and
-- every row an image from before it writes (it names its columns), is taken
-- the way it was, first come first served, never behind a bulk send. A
-- constant default is stored in the catalog, so adding the column rewrites
-- nothing; an old image reads and writes the queue as before.
--
-- lock_timeout as in 20261009120000: the ACCESS EXCLUSIVE lock is held for an
-- instant, but waits behind any transaction holding the table, and every queue
-- query waits behind it; past 5 seconds the migration fails, rolled back, and
-- the old task keeps sending (docs/database-migrations.md).
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN priority SMALLINT NOT NULL DEFAULT 1;

RESET lock_timeout;
