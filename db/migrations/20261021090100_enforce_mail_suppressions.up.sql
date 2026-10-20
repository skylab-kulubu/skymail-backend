-- The suppression list is applied when a send is queued (hub spec
-- skymail-bounce-isleme §4, ticket 05; README, "Geri dönen posta ve
-- bastırma listesi"):
--
-- mail_queue.suppressed_but_sent: account mail (a keycloak.* or account.*
--   template: a login or sign-up code, a verification, a password reset) to a
--   suppressed address is sent anyway, and its row says so. Any other mail to
--   a suppressed address is written failed with 'suppressed: <source> <code>'
--   and never sent.
-- mail_suppression_removals: who removed a suppression, when, and what it
--   said (source, code, count, first seen). No address.
-- mail_task_status(): a suppressed recipient is skipped, not failed, as a
--   reserved_domain one (20261020150000): no mail could reach it, and one of
--   them must not make a list send read failed. A send whose every recipient
--   was skipped (a single mail to a suppressed address) reads failed: nothing
--   went out.
--
-- A column with a constant default, a new table and a function: an image from
-- before runs on it as it is (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

ALTER TABLE mail_queue
    ADD COLUMN suppressed_but_sent BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN mail_queue.suppressed_but_sent IS
    'Account mail (keycloak.*, account.*) queued to a suppressed address: sent anyway, flagged.';

CREATE TABLE mail_suppression_removals
(
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    removed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_sub   TEXT        NOT NULL,
    source      TEXT        NOT NULL,
    reason_code TEXT        NOT NULL,
    count       INTEGER     NOT NULL,
    first_seen  TIMESTAMPTZ NOT NULL
);

COMMENT ON TABLE mail_suppression_removals IS
    'Who removed a suppression and when, with what it said (source, code, count, first seen). No address; Account erasure replaces the actor.';

CREATE OR REPLACE FUNCTION mail_task_status(for_task UUID) RETURNS TEXT
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT CASE
           WHEN EXISTS (SELECT 1
                        FROM mail_queue
                        WHERE task_id = for_task
                          AND status = 'failed'
                          AND COALESCE(error, '') NOT LIKE 'reserved\_domain:%'
                          AND COALESCE(error, '') NOT LIKE 'suppressed:%')
               THEN 'failed'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status IN ('pending', 'processing'))
               THEN 'sending'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task AND status = 'sent')
               THEN 'sent'
           WHEN EXISTS (SELECT 1 FROM mail_queue WHERE task_id = for_task)
               THEN 'failed'
           WHEN (SELECT created_at FROM mail_tasks WHERE id = for_task) > now() - INTERVAL '1 minute'
               THEN 'sending'
           ELSE 'failed'
           END
$$;

RESET lock_timeout;
