-- Bounces and the suppression list (hub spec skymail-bounce-isleme §3–§5,
-- tickets 03 and iletisim-servisi 23).
--
-- mail_bounces: what a delivery status notification (an NDR) or a relay's
--   refusal said about one queue row: hard (the address does not exist),
--   soft (any other failure) or delay. No address and no text: the address is
--   the queue row's, and of a report only its codes are kept (dsn package).
--   ndr_id_sha256 is the SHA-256 of the report's own id (Graph
--   internetMessageId), so one report is recorded once; NULL for a relay's
--   refusal. recipient_matched is false when the report named another address
--   than the row's (a forwarding or an alias): recorded, never suppressed.
--   Kept a year (the content sweep deletes older rows), and with the queue
--   row (ON DELETE CASCADE).
-- mail_suppressions: an address one permanent failure was recorded for. Mail
--   to it is not sent again (ticket 05), except account mail, which is sent
--   and flagged (exempt_flagged_at). Nothing removes a row by itself: an
--   operator does, or Account erasure. address is lower-case, the key; id is
--   what the operator API names a row by.
--
-- New tables only: an image from before neither reads nor writes them
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_bounces
(
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    queue_id          UUID        NOT NULL REFERENCES mail_queue (id) ON DELETE CASCADE,
    source            TEXT        NOT NULL,
    kind              TEXT        NOT NULL,
    action            TEXT        NOT NULL,
    status            TEXT,
    smtp_code         SMALLINT,
    exo_as_code       TEXT,
    recipient_matched BOOLEAN     NOT NULL DEFAULT true,
    ndr_id_sha256     TEXT,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT mail_bounces_source CHECK (source IN ('dsn', 'smtp')),
    CONSTRAINT mail_bounces_kind CHECK (kind IN ('hard', 'soft', 'delay')),
    CONSTRAINT mail_bounces_action CHECK (action IN ('failed', 'delayed')),
    CONSTRAINT mail_bounces_status CHECK (status ~ '^[245]\.[0-9]{1,3}\.[0-9]{1,3}$'),
    CONSTRAINT mail_bounces_smtp_code CHECK (smtp_code BETWEEN 200 AND 599),
    CONSTRAINT mail_bounces_exo_as_code CHECK (exo_as_code ~ '^[0-9]{1,12}$'),
    CONSTRAINT mail_bounces_ndr_id CHECK ((source = 'dsn') = (ndr_id_sha256 IS NOT NULL)
        AND (ndr_id_sha256 IS NULL OR ndr_id_sha256 ~ '^[0-9a-f]{64}$')),
    -- A report never suppresses an address it does not name.
    CONSTRAINT mail_bounces_matched CHECK (recipient_matched OR source = 'dsn'),
    CONSTRAINT mail_bounces_ndr_once UNIQUE (ndr_id_sha256)
);

CREATE INDEX idx_mail_bounces_queue ON mail_bounces (queue_id);
CREATE INDEX idx_mail_bounces_received ON mail_bounces (received_at);

COMMENT ON TABLE mail_bounces IS
    'What a delivery status notification or a relay''s refusal said about a queue row: kind, action and codes. No address, no text. Kept a year.';
COMMENT ON COLUMN mail_bounces.ndr_id_sha256 IS
    'SHA-256 (hex) of the report''s own id (Graph internetMessageId): one report is recorded once. NULL for a relay''s refusal.';
COMMENT ON COLUMN mail_bounces.recipient_matched IS
    'Whether the report named the row''s own address; a report that named another (a forwarding, an alias) never suppresses.';

CREATE TABLE mail_suppressions
(
    address           TEXT PRIMARY KEY,
    -- What the operator API names a row by: the address is never in a URL
    -- (an error's log line carries the path).
    id                UUID        NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    recipient_sub     UUID,
    source            TEXT        NOT NULL,
    reason_code       TEXT        NOT NULL,
    first_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen         TIMESTAMPTZ NOT NULL DEFAULT now(),
    count             INTEGER     NOT NULL DEFAULT 1,
    last_queue_id     UUID REFERENCES mail_queue (id) ON DELETE SET NULL,
    exempt_flagged_at TIMESTAMPTZ,
    CONSTRAINT mail_suppressions_address CHECK (address = lower(btrim(address)) AND address LIKE '_%@_%'),
    CONSTRAINT mail_suppressions_source CHECK (source IN ('dsn', 'smtp')),
    CONSTRAINT mail_suppressions_reason_code CHECK (reason_code ~ '^[A-Z0-9 .()]{1,40}$'),
    CONSTRAINT mail_suppressions_count CHECK (count >= 1)
);

CREATE INDEX idx_mail_suppressions_sub ON mail_suppressions (recipient_sub) WHERE recipient_sub IS NOT NULL;
CREATE INDEX idx_mail_suppressions_last_queue ON mail_suppressions (last_queue_id) WHERE last_queue_id IS NOT NULL;

COMMENT ON TABLE mail_suppressions IS
    'Addresses a permanent failure was recorded for: mail to them is not sent, account mail is sent and flagged. Removed by an operator or Account erasure, never by itself.';
COMMENT ON COLUMN mail_suppressions.reason_code IS
    'The codes of the last permanent failure: 5.1.10, 5.4.1 AS(201806281), RCPT TO 550 5.1.1. Never the text.';
COMMENT ON COLUMN mail_suppressions.exempt_flagged_at IS
    'When account mail (keycloak.*, account.*) was last sent to the address anyway, flagged.';

RESET lock_timeout;
