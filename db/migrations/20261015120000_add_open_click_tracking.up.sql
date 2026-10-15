-- Open and click tracking (hub ticket iletisim-servisi 14; design note
-- notes/iletisim-servisi-arastirma/07 §4). A bulk send whose sender asked for
-- it, while SKYMAIL_TRACKING=on, gives each recipient core lets be tracked a
-- random token of their own; the mail goes out with a pixel and its links
-- rewritten to SkyMail's /t/ addresses, put in at SMTP time, so the mail
-- kept (body_html) stays as rendered. Nothing here holds an IP address, a
-- client's User-Agent or a referrer, and no table ever will.
--
-- mail_tasks.tracking_requested: the sender asked for tracking and the flag
--   was on when the send was queued. tracking_closed_at: when retention closed
--   the send's tracking (90 days on: events rolled up and deleted, tokens
--   closed); null while it is open.
-- mail_approvals.tracking_requested: the submitter asked for tracking; the
--   approved send asks for it if the flag is on then.
-- mail_queue.tracking_token: the recipient's token, 16 random bytes; null for
--   a row that is not tracked. tracking_disabled_at: when the token was
--   closed (withdrawal, account erasure, retention, the flag turned off before
--   the mail went out); a closed token records nothing and its links still
--   redirect. tracking_skip_reason: why a row of a send that asked for
--   tracking is not tracked. sent_at: when the relay took the mail.
-- mail_task_links: the links of a send that are the same for every
--   recipient, each with its own random token; the only addresses a click
--   redirects to. url is emptied (url_cleared_reason) a year on or when it
--   names an erased person.
-- mail_tracking_events: one opening or click: the row, when, the client's
--   class as the classifier read it from the User-Agent in memory.
-- mail_task_tracking_totals: a send's counts without people, kept after its
--   events go, only for sends with at least 10 tracked recipients (T4).
--
-- Every column is nullable or has a constant default, and every check on the
-- queue is NOT VALID: nothing is rewritten or read under the ACCESS EXCLUSIVE
-- lock, and an image from before names its columns (sqlc spells out *) and
-- goes on as before (docs/database-migrations.md, expand). The queue's and the
-- sends' indexes come in the next migrations, CONCURRENTLY, one per file.
--
-- lock_timeout as in 20261014150000: the locks are held for an instant, but
-- wait behind any transaction holding the tables; past 5 seconds the
-- migration fails, rolled back, and the old task keeps sending.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN tracking_requested BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN tracking_closed_at TIMESTAMPTZ;

ALTER TABLE mail_approvals
    ADD COLUMN tracking_requested BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE mail_queue
    ADD COLUMN tracking_token       BYTEA,
    ADD COLUMN tracking_disabled_at TIMESTAMPTZ,
    ADD COLUMN tracking_skip_reason TEXT,
    ADD COLUMN sent_at              TIMESTAMPTZ,
    ADD CONSTRAINT mail_queue_tracking_token_length
        CHECK (tracking_token IS NULL OR octet_length(tracking_token) = 16) NOT VALID,
    ADD CONSTRAINT mail_queue_tracking_skip_reason_value
        CHECK (tracking_skip_reason IN ('flag_off', 'transactional', 'no_account', 'preference_off', 'not_member',
                                        'inactive', 'preference_unavailable', 'not_allowed')) NOT VALID;

COMMENT ON COLUMN mail_tasks.tracking_requested IS
    'The sender asked for open and click tracking and SKYMAIL_TRACKING was on when the send was queued.';
COMMENT ON COLUMN mail_tasks.tracking_closed_at IS
    'When retention closed the send''s tracking: events rolled up and deleted, tokens closed. Null while open.';
COMMENT ON COLUMN mail_approvals.tracking_requested IS
    'The submitter asked for open and click tracking; the approved send asks for it if SKYMAIL_TRACKING is on then.';
COMMENT ON COLUMN mail_queue.tracking_token IS
    'The recipient''s tracking token, 16 random bytes; null for a row that is not tracked.';
COMMENT ON COLUMN mail_queue.tracking_disabled_at IS
    'When the token was closed (withdrawal, erasure, retention, the flag off before sending); a closed token records nothing.';
COMMENT ON COLUMN mail_queue.tracking_skip_reason IS
    'Why a row of a send that asked for tracking is not tracked.';
COMMENT ON COLUMN mail_queue.sent_at IS
    'When the relay took the mail; null for a row not sent, or sent by an image from before it.';

CREATE TABLE mail_task_links
(
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id            UUID        NOT NULL REFERENCES mail_tasks (id) ON DELETE CASCADE,
    link_token         BYTEA       NOT NULL UNIQUE,
    url                TEXT,
    position           SMALLINT    NOT NULL,
    url_cleared_reason TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT mail_task_links_token_length CHECK (octet_length(link_token) = 8),
    CONSTRAINT mail_task_links_position CHECK (position >= 0),
    CONSTRAINT mail_task_links_url_length CHECK (url IS NULL OR char_length(url) BETWEEN 1 AND 2048),
    CONSTRAINT mail_task_links_cleared CHECK ((url IS NULL) = (url_cleared_reason IS NOT NULL)),
    CONSTRAINT mail_task_links_cleared_reason CHECK (url_cleared_reason IN ('expired', 'erased')),
    CONSTRAINT mail_task_links_task_position UNIQUE (task_id, position),
    CONSTRAINT mail_task_links_task_url UNIQUE (task_id, url)
);

COMMENT ON TABLE mail_task_links IS
    'A send''s links that are the same for every recipient, each with its random token: the only addresses /t/c redirects to.';

CREATE TABLE mail_tracking_events
(
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    queue_id           UUID        NOT NULL REFERENCES mail_queue (id) ON DELETE CASCADE,
    task_id            UUID        NOT NULL,
    kind               TEXT        NOT NULL,
    link_id            BIGINT REFERENCES mail_task_links (id) ON DELETE CASCADE,
    occurred_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_class       TEXT        NOT NULL,
    automated_likely   BOOLEAN     NOT NULL,
    classifier_version SMALLINT    NOT NULL,
    CONSTRAINT mail_tracking_events_kind CHECK (kind IN ('open', 'click')),
    CONSTRAINT mail_tracking_events_click_link CHECK ((kind = 'click') = (link_id IS NOT NULL)),
    CONSTRAINT mail_tracking_events_source_class
        CHECK (source_class IN ('apple_proxy_likely', 'google_proxy', 'yahoo_proxy', 'microsoft', 'scanner_likely',
                                'mail_client', 'browser', 'unknown'))
);

CREATE INDEX idx_mail_tracking_events_task ON mail_tracking_events (task_id, kind, occurred_at);
CREATE INDEX idx_mail_tracking_events_queue ON mail_tracking_events (queue_id, occurred_at);
CREATE INDEX idx_mail_tracking_events_link ON mail_tracking_events (link_id) WHERE link_id IS NOT NULL;

COMMENT ON TABLE mail_tracking_events IS
    'One opening or click of a tracked mail: the row, when, the client''s class. No IP address, User-Agent or referrer.';

CREATE TABLE mail_task_tracking_totals
(
    task_id                   UUID PRIMARY KEY REFERENCES mail_tasks (id) ON DELETE CASCADE,
    tracked_recipients        INT         NOT NULL,
    untracked_by_reason       JSONB       NOT NULL DEFAULT '{}',
    unique_opens              INT         NOT NULL,
    unique_likely_human_opens INT         NOT NULL,
    unique_clickers           INT         NOT NULL,
    clicks_by_link            JSONB       NOT NULL DEFAULT '[]',
    rolled_up_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT mail_task_tracking_totals_anonymous CHECK (tracked_recipients >= 10)
);

COMMENT ON TABLE mail_task_tracking_totals IS
    'A send''s tracking counts without people, kept after its events go; only sends with at least 10 tracked recipients.';

RESET lock_timeout;
