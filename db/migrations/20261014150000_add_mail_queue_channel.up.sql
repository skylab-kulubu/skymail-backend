-- A queue row is one delivery to one person over one channel (hub ticket
-- iletisim-servisi 02). The engine (the lanes, the lease and its fencing,
-- the retries, the send budget) does not care how a row goes out; the
-- worker hands it to its channel's sender.
--
-- mail_queue.channel: how the row goes out. Email is the only channel yet;
-- every row already queued, and every row an image from before this writes,
-- is an e-mail (the default). A channel joins the check in its own ticket
-- (inbox, push, sms). recipient_email keeps its name and is read as the
-- channel's address; a rename is a later contract.
--
-- mail_queue.recipient_sub: the recipient's Keycloak subject, when the sender
-- knows it (a Keycloak group's member, in a direct or an approved send to the
-- group). Null when only an address is known: a mailing list's recipient, a
-- person named by address, a single mail (POST /mail_tasks/single takes an
-- address; the Mail onayı notifications go that way too).
--
-- A constant default is stored in the catalog and a nullable column has
-- none, so adding them rewrites nothing; an image from before them names its
-- columns (sqlc spells out *) and reads and writes the queue as before. The
-- check is NOT VALID: it holds for every row written from now on without
-- reading the table under the ACCESS EXCLUSIVE lock, and the rows already
-- there are e-mails by the default (docs/database-migrations.md, expand).
--
-- lock_timeout as in 20261009120000: the lock is held for an instant, but
-- waits behind any transaction holding the table, and every queue query
-- waits behind it; past 5 seconds the migration fails, rolled back, and the
-- old task keeps sending (docs/database-migrations.md).
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN channel TEXT NOT NULL DEFAULT 'email',
    ADD COLUMN recipient_sub UUID,
    ADD CONSTRAINT mail_queue_channel_check CHECK (channel IN ('email')) NOT VALID;

COMMENT ON COLUMN mail_queue.channel IS
    'How the row goes out: email (recipient_email is the address). Other channels join mail_queue_channel_check in their own migration.';

COMMENT ON COLUMN mail_queue.recipient_sub IS
    'The recipient''s Keycloak subject when the sender knew it; null when only an address was known.';

RESET lock_timeout;
