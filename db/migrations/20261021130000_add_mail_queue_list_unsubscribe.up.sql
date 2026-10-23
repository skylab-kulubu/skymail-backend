-- One-click unsubscribe (RFC 8058; hub ticket skymail-bounce-isleme 11):
-- a bulk mail that core gave an unsubscribe link for carries it in
-- List-Unsubscribe and List-Unsubscribe-Post, from the duyuru@ mailbox only,
-- while SKYMAIL_LIST_UNSUBSCRIBE is on.
--
-- mail_queue.list_unsubscribe_url: the recipient's own link, as core signed
-- it (core's /v1/communication-preferences/unsubscribe?t=…; SkyMail never
-- holds the key). Null for every other row. Never served by the API; emptied
-- with the row's personal content (retention, the masking deadline, a send
-- that is not kept), and deleted with the row on Account erasure.
--
-- A nullable column has no default, so adding it rewrites nothing; an image
-- from before it names its columns (sqlc spells out *) and reads and writes
-- the queue as before. The check is NOT VALID: it holds for every row written
-- from now on without reading the table under the ACCESS EXCLUSIVE lock, and
-- every row already there is null (docs/database-migrations.md, expand).
--
-- lock_timeout as in 20261014150000: the lock is held for an instant, but
-- waits behind any transaction holding the table, and every queue query
-- waits behind it; past 5 seconds the migration fails, rolled back, and the
-- old task keeps sending.
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN list_unsubscribe_url TEXT,
    ADD CONSTRAINT mail_queue_list_unsubscribe_url_check CHECK (
        list_unsubscribe_url IS NULL
            OR (list_unsubscribe_url LIKE 'https://%' AND length(list_unsubscribe_url) <= 960)
        ) NOT VALID;

COMMENT ON COLUMN mail_queue.list_unsubscribe_url IS
    'The recipient''s one-click unsubscribe link, as core signed it; null for a row without one. Never served.';

RESET lock_timeout;
