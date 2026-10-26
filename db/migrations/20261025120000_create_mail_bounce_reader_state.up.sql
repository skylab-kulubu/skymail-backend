-- The NDR reader's place in each sender mailbox (hub ticket
-- skymail-bounce-isleme 04; README, "Geri dönen posta ve bastırma listesi").
--
-- One row a mailbox and mode: the Graph delta link the next poll starts from,
-- when it last polled, the last poll's error (an HTTP status or a code, never
-- a report's text) and what the reports it read came to, counted since the
-- row was made. dry-run keeps a row of its own, so turning the reader on
-- reads the inbox again from the start and records what the dry run only
-- counted. The mailbox is the club's own sender mailbox, not a person's: no
-- personal data.
--
-- A new table only: an image from before neither reads nor writes it
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_bounce_reader_state
(
    mailbox         TEXT        NOT NULL,
    mode            TEXT        NOT NULL,
    delta_link      TEXT,
    last_polled_at  TIMESTAMPTZ,
    last_error      TEXT,
    reports         BIGINT      NOT NULL DEFAULT 0,
    not_dsn         BIGINT      NOT NULL DEFAULT 0,
    unmatched       BIGINT      NOT NULL DEFAULT 0,
    hard            BIGINT      NOT NULL DEFAULT 0,
    soft            BIGINT      NOT NULL DEFAULT 0,
    delay           BIGINT      NOT NULL DEFAULT 0,
    other_recipient BIGINT      NOT NULL DEFAULT 0,
    duplicate       BIGINT      NOT NULL DEFAULT 0,
    no_failure      BIGINT      NOT NULL DEFAULT 0,
    unreadable      BIGINT      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (mailbox, mode),
    CONSTRAINT mail_bounce_reader_state_mode CHECK (mode IN ('dry-run', 'on')),
    CONSTRAINT mail_bounce_reader_state_mailbox CHECK (mailbox = lower(mailbox) AND mailbox LIKE '_%@_%'),
    CONSTRAINT mail_bounce_reader_state_delta_link CHECK (delta_link ~ '^https?://'),
    CONSTRAINT mail_bounce_reader_state_error CHECK (char_length(last_error) <= 200)
);

COMMENT ON TABLE mail_bounce_reader_state IS
    'The NDR reader''s Graph delta link, last poll and counts, one row a sender mailbox and mode. No personal data.';
