-- The in-app inbox (hub ticket iletisim-servisi 05; ADR 0070): the inbox
-- channel and what a person's "Bildirimlerim" reads.
--
-- notification_types.default_channels may hold inbox. A type whose default
-- channels hold it writes an inbox item for every recipient, whatever the
-- person's allowed_channels say (the inbox does not look at preferences);
-- e-mail still does.
--
-- notification_variants: a type's text on a channel other than e-mail (the
--   e-mail variant is the template of the type's key). Inbox only: a title,
--   a summary and an in-app link, each a Go text/template over the
--   notification's variables. The Template seed writes it with the type
--   (PUT /v1/notification_types/{key}); push and SMS join the check with
--   their tickets (12, 13).
--
-- inbox_items: one item in one person's inbox. It is not a copy of the
--   ticket, certificate or news it points to: the variant's text rendered
--   once for the notification, and the link. user_id is the person's
--   Keycloak subject. seen_at, read_at and archived_at are the person's own
--   marks, kept here so every device sees them. expires_at is when the item
--   goes: 90 days after it was written, or sooner when the notification said
--   its source ends sooner (an event that is over). Reads leave out what is
--   past it; the inbox sweep deletes it, and account erasure deletes a
--   person's items whatever their age. An item is written in the
--   notification's own transaction, never through the mail queue: nothing
--   leaves the database, so the queue's retries, lanes and send budget have
--   nothing to do.
--
-- New tables, and a check that lets one more value in: an image from before
-- this one neither reads nor writes the tables, and writes only email types
-- (docs/database-migrations.md, expand). Rolling back to such an image with a
-- type whose default channels hold inbox would e-mail its recipients once
-- per channel: take inbox out of every type first (README, inbox).
SET lock_timeout = '5s';

ALTER TABLE notification_types
    DROP CONSTRAINT notification_types_channels,
    ADD CONSTRAINT notification_types_channels CHECK (cardinality(default_channels) >= 1
        AND default_channels <@ ARRAY ['email', 'inbox']::text[]);

CREATE TABLE notification_variants
(
    type       TEXT        NOT NULL REFERENCES notification_types (key) ON DELETE CASCADE,
    channel    TEXT        NOT NULL
        CONSTRAINT notification_variants_channel CHECK (channel IN ('inbox')),
    title      TEXT        NOT NULL
        CONSTRAINT notification_variants_title CHECK (btrim(title) <> '' AND char_length(title) <= 1000),
    summary    TEXT        NOT NULL DEFAULT ''
        CONSTRAINT notification_variants_summary CHECK (char_length(summary) <= 4000),
    link       TEXT        NOT NULL DEFAULT ''
        CONSTRAINT notification_variants_link CHECK (char_length(link) <= 2048),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (type, channel)
);

COMMENT ON TABLE notification_variants IS
    'A notification type''s text on a channel other than e-mail (inbox: title, summary, in-app link; Go text/template over the variables). The e-mail variant is the template of the type''s key.';

CREATE TABLE inbox_items
(
    id                UUID PRIMARY KEY     DEFAULT gen_random_uuid(),
    user_id           UUID        NOT NULL,
    notification_id   UUID        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    notification_type TEXT        NOT NULL,
    title             TEXT        NOT NULL,
    summary           TEXT        NOT NULL DEFAULT '',
    link              TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    seen_at           TIMESTAMPTZ,
    read_at           TIMESTAMPTZ,
    archived_at       TIMESTAMPTZ,
    expires_at        TIMESTAMPTZ NOT NULL,
    CONSTRAINT inbox_items_once UNIQUE (notification_id, user_id),
    CONSTRAINT inbox_items_ninety_days CHECK (expires_at <= created_at + interval '90 days')
);

-- A person's inbox, newest first (the cursor is created_at, id).
CREATE INDEX idx_inbox_items_user_feed ON inbox_items (user_id, created_at DESC, id DESC);
-- The sweep.
CREATE INDEX idx_inbox_items_expires_at ON inbox_items (expires_at);

COMMENT ON TABLE inbox_items IS
    'One item in one person''s in-app inbox: the type''s inbox variant rendered for one notification, the person''s seen/read/archived marks; gone at expires_at (at most 90 days) or with the person''s account erasure.';
COMMENT ON COLUMN inbox_items.user_id IS
    'The person''s Keycloak subject. Only that person reads or marks the item.';

RESET lock_timeout;
