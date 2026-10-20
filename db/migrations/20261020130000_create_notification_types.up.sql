-- Notification types and POST /v1/notifications (hub ticket iletisim-servisi
-- 03; ADR 0069 draft: core says what happened and to whom, SkyMail picks the
-- channel and the text).
--
-- notification_types: what core may send a person by POST /v1/notifications.
--   key is the Template key's alphabet (event.cancelled); a type's e-mail
--   variant is the template of the same key. category is the preference
--   category the type belongs to (null: none); required types (a login code,
--   a ticket, a certificate) do not look at preferences, optional ones go
--   only where core allows each person (allowedChannels). default_channels
--   are the channels a type goes by; lane is the queue lane its deliveries
--   wait in (system: never behind a bulk send; bulk: at most 70% of the
--   workers). The Template seed writes them (PUT /v1/notification_types/{key});
--   archived_at takes a type out of use.
--
-- templates.channel: the variant a template is. Today every template is the
--   e-mail variant ('email', the CHECK); the other channels' variants come
--   with their tickets (05 inbox). The unique index stays on key until a
--   second channel exists: an image from before this one upserts the seed
--   ON CONFLICT (key).
--
-- notifications: one accepted POST /v1/notifications: its type, who sent it,
--   its Idempotency-Key with a SHA-256 of the request, where it came from
--   (source kind and ref) and the answer it got, counts only (no person, no
--   address). The same key from the same caller answers the first answer
--   again; another request with it is refused. mail_tasks.notification_id is
--   the e-mail send a notification queued.
--
-- New tables, a defaulted column and a nullable one: an image from before this
-- one neither reads nor writes them (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE notification_types
(
    key              TEXT PRIMARY KEY
        CONSTRAINT notification_types_key_format CHECK (key ~ '^[a-z0-9][a-z0-9.-]{1,62}[a-z0-9]$'),
    category         TEXT
        CONSTRAINT notification_types_category_format CHECK (category IS NULL OR category ~ '^[a-z0-9][a-z0-9._-]{0,62}[a-z0-9]$'),
    required         BOOLEAN     NOT NULL DEFAULT false,
    default_channels TEXT[]      NOT NULL DEFAULT '{email}'
        CONSTRAINT notification_types_channels CHECK (cardinality(default_channels) >= 1
            AND default_channels <@ ARRAY ['email']::text[]),
    lane             TEXT        NOT NULL DEFAULT 'bulk'
        CONSTRAINT notification_types_lane CHECK (lane IN ('system', 'bulk')),
    archived_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE notification_types IS
    'What core may send a person by POST /v1/notifications: category (null: none), required (preferences not looked at), default channels, queue lane. The e-mail variant is the template of the same key.';

ALTER TABLE templates
    ADD COLUMN channel TEXT NOT NULL DEFAULT 'email'
        CONSTRAINT templates_channel CHECK (channel IN ('email'));

COMMENT ON COLUMN templates.channel IS
    'The channel variant the template is: email (subject, HTML, plain text). Other channels'' variants come with their channel.';

CREATE TABLE notifications
(
    id              UUID PRIMARY KEY     DEFAULT gen_random_uuid(),
    type            TEXT        NOT NULL REFERENCES notification_types (key),
    sent_by         TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL
        CONSTRAINT notifications_idempotency_key_length CHECK (char_length(idempotency_key) BETWEEN 1 AND 255),
    request_sha256  TEXT        NOT NULL,
    source_kind     TEXT,
    source_ref      TEXT,
    result          JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notifications_idempotency UNIQUE (sent_by, idempotency_key)
);

CREATE INDEX idx_notifications_type ON notifications (type, created_at);

COMMENT ON TABLE notifications IS
    'One accepted POST /v1/notifications: type, caller, Idempotency-Key and a SHA-256 of the request, source, and its answer (counts only).';

ALTER TABLE mail_tasks
    ADD COLUMN notification_id UUID REFERENCES notifications (id);

RESET lock_timeout;
