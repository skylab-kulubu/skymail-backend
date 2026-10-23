-- A send's purpose and category (ADR 0072 decision 2; hub ticket
-- abonelik-cikisi 03; README, "Gönderim amacı ve kategori"). The purpose, not
-- the queue priority, says which message stream a mail is in:
--
--   purpose: islem (the transactional stream: hesap@ and bilgi@; no
--     preference, no unsubscribe record, no List-Unsubscribe; tracked by the
--     tracking rules, as any mail) or
--     duyuru (the broadcast stream: duyuru@; a category, the preference, the
--     unsubscribe record, the one-click header and a visible link). NULL is a
--     row from before, or one queued while SKYMAIL_MAIL_PURPOSE was off: read
--     by the rule of before, the bulk priority duyuru, any other islem.
--   category: core's category of a duyuru send, club:duyuru, club:haber,
--     team:<Keycloak group id>:duyuru or team:<id>:haber; NULL for any other.
--   transactional_statement: on an islem send an operator chose, their
--     statement that the mail is about the recipient's own application or
--     transaction (true); NULL otherwise (a service's mail, a send from an
--     older client, any duyuru send).
--   commercial_content_absent: on a duyuru send, the sender's statement that
--     it carries no commercial content (ADR 0072 decision 9) as they gave it;
--     NULL when they gave none, and on every islem send.
--
-- mail_approvals keeps the same four for the send it would queue.
--
-- Expand only (docs/database-migrations.md): nullable columns without a
-- default rewrite nothing, and an image from before names its columns (sqlc
-- spells out *), so it reads and writes both tables as before and leaves the
-- four NULL, which the checks allow. The checks are NOT VALID: they hold for
-- every row written from now on without reading the tables under the ACCESS
-- EXCLUSIVE lock, and every row already there is NULL in all four.
--
-- lock_timeout as in 20261014150000: the locks are held for an instant, but
-- wait behind any transaction holding the tables; past 5 seconds the
-- migration fails, rolled back, and the old task keeps sending.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN purpose                   TEXT,
    ADD COLUMN category                  TEXT,
    ADD COLUMN transactional_statement   BOOLEAN,
    ADD COLUMN commercial_content_absent BOOLEAN,
    ADD CONSTRAINT mail_tasks_purpose_check CHECK (purpose IS NULL OR purpose IN ('islem', 'duyuru')) NOT VALID,
    ADD CONSTRAINT mail_tasks_category_check CHECK (
        category IS NULL
            OR category ~ '^(club|team:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}):(duyuru|haber)$'
        ) NOT VALID,
    -- Each field belongs to its purpose: a duyuru send has a category and no
    -- transactional statement, an islem send neither a category nor a
    -- commercial statement, a send without a purpose none of the three.
    ADD CONSTRAINT mail_tasks_purpose_fields_check CHECK (
        CASE purpose
            WHEN 'duyuru' THEN category IS NOT NULL AND transactional_statement IS NULL
            WHEN 'islem' THEN category IS NULL AND commercial_content_absent IS NULL
            ELSE category IS NULL AND transactional_statement IS NULL AND commercial_content_absent IS NULL
            END
        ) NOT VALID,
    -- A send to a list or a Keycloak group is never islem (decision 2). An
    -- older image never writes purpose, so it cannot break this; it is not
    -- put on mail_approvals, whose audience an older image's resubmit
    -- rewrites (the approval queues such a request as duyuru instead).
    ADD CONSTRAINT mail_tasks_purpose_audience_check CHECK (purpose IS DISTINCT FROM 'islem' OR mail_list_id IS NULL) NOT VALID;

ALTER TABLE mail_approvals
    ADD COLUMN purpose                   TEXT,
    ADD COLUMN category                  TEXT,
    ADD COLUMN transactional_statement   BOOLEAN,
    ADD COLUMN commercial_content_absent BOOLEAN,
    ADD CONSTRAINT mail_approvals_purpose_check CHECK (purpose IS NULL OR purpose IN ('islem', 'duyuru')) NOT VALID,
    ADD CONSTRAINT mail_approvals_category_check CHECK (
        category IS NULL
            OR category ~ '^(club|team:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}):(duyuru|haber)$'
        ) NOT VALID,
    ADD CONSTRAINT mail_approvals_purpose_fields_check CHECK (
        CASE purpose
            WHEN 'duyuru' THEN category IS NOT NULL AND transactional_statement IS NULL
            WHEN 'islem' THEN category IS NULL AND commercial_content_absent IS NULL
            ELSE category IS NULL AND transactional_statement IS NULL AND commercial_content_absent IS NULL
            END
        ) NOT VALID;

COMMENT ON COLUMN mail_tasks.purpose IS
    'islem or duyuru: the message stream (ADR 0072 decision 2). NULL: read by the rule of before (the bulk priority duyuru, any other islem).';
COMMENT ON COLUMN mail_tasks.category IS
    'core''s category of a duyuru send (club:duyuru, club:haber, team:<group id>:duyuru|haber); NULL for any other.';
COMMENT ON COLUMN mail_approvals.purpose IS
    'The purpose of the send the request would queue (mail_tasks.purpose).';

RESET lock_timeout;
