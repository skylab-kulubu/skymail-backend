-- The list's own event statement (KVKK aydınlatma s6 row 15 and §5.14; hub
-- ticket kvkk-s6-yayin-oncesi 12, Yusuf's T3 decision, 2026-10-24):
--
--   own_event_statement: on a duyuru send to a list or a Keycloak group, the
--     sender's statement that the mail is about the event or the work the
--     list was made for (a changed session time, a cancellation). Kept with
--     the send's sender and time as the statement's record, as
--     transactional_statement is. While SKYMAIL_LIST_CONSENT is on, such a
--     mail goes to a list's address without an account without asking for
--     its announcement consent (5/2-c), still in the duyuru stream with its
--     way out. NULL when the sender gave none, and on every other send.
--
-- mail_approvals keeps it for the send it would queue.
--
-- Expand only (docs/database-migrations.md): a nullable column without a
-- default rewrites nothing, and an image from before names its columns, so it
-- leaves the column NULL, which the checks allow. The checks are NOT VALID,
-- as in 20261023100000.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN own_event_statement BOOLEAN,
    ADD CONSTRAINT mail_tasks_own_event_statement_check CHECK (own_event_statement IS NULL OR purpose = 'duyuru') NOT VALID;

ALTER TABLE mail_approvals
    ADD COLUMN own_event_statement BOOLEAN,
    ADD CONSTRAINT mail_approvals_own_event_statement_check CHECK (own_event_statement IS NULL OR purpose = 'duyuru') NOT VALID;

COMMENT ON COLUMN mail_tasks.own_event_statement IS
    'On a duyuru send to a list or a group: the sender''s statement that the mail is about the list''s own event or work (s6 row 15); NULL otherwise.';

RESET lock_timeout;
