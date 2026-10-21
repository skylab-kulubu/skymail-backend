-- The unsubscribe record of the duyuru stream (ADR 0072 decisions 4–7; hub
-- ticket abonelik-cikisi 02; README, "Tek tık çıkış ucu"):
--
-- mail_unsubscribes: an address that left a broadcast stream through
--   SkyMail's own link (the one-click POST or the page's button). Mail of
--   that stream is not sent to it again; mail of the islem stream (account
--   mail, Forms confirmations, notices) is, and the bounce list is applied as
--   ever. One row per (address, stream). address is lower-case; id is what
--   the operator API names a row by, so the address is never in a URL.
--   Nothing removes a row by itself: the person resubscribes, an operator
--   with skymail:unsubscribes:reactivate reverses it at the person's request
--   (or after a mistake), or Account erasure deletes it.
-- mail_unsubscribe_reactivations: who reversed an unsubscribe (the person
--   themself, or an operator with a reason), when, and what the record said
--   (stream, source, first date). No address.
--
-- A sibling of mail_suppressions, not a column on it: an image from before
-- reads every mail_suppressions row as a bounce and would stop every mail to
-- an unsubscribed address, Forms confirmations included. It never reads these
-- tables. New tables only (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_unsubscribes
(
    address    TEXT        NOT NULL,
    stream     TEXT        NOT NULL,
    id         UUID        NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    source     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (address, stream),
    CONSTRAINT mail_unsubscribes_address CHECK (address = lower(btrim(address)) AND address LIKE '_%@_%'
        AND length(address) <= 254),
    -- The broadcast streams; islem never has an unsubscribe record.
    CONSTRAINT mail_unsubscribes_stream CHECK (stream IN ('duyuru')),
    -- link: SkyMail's one-click link or its page. A complaint feedback loop
    -- (ticket 12) would add its own source.
    CONSTRAINT mail_unsubscribes_source CHECK (source IN ('link'))
);

CREATE INDEX idx_mail_unsubscribes_created ON mail_unsubscribes (created_at);

COMMENT ON TABLE mail_unsubscribes IS
    'Addresses that left a broadcast stream (duyuru) through SkyMail''s link: that stream''s mail is not sent to them. Removed by the person, by an operator at their request, or by Account erasure; never by itself.';

CREATE TABLE mail_unsubscribe_reactivations
(
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    reactivated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- recipient: the person, on their link's page; operator: the panel.
    actor           TEXT        NOT NULL,
    actor_sub       TEXT,
    reason          TEXT,
    stream          TEXT        NOT NULL,
    source          TEXT        NOT NULL,
    unsubscribed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT mail_unsubscribe_reactivations_actor CHECK (
        (actor = 'recipient' AND actor_sub IS NULL AND reason IS NULL)
            OR (actor = 'operator' AND actor_sub IS NOT NULL AND reason IS NOT NULL))
);

CREATE INDEX idx_mail_unsubscribe_reactivations_at ON mail_unsubscribe_reactivations (reactivated_at);

COMMENT ON TABLE mail_unsubscribe_reactivations IS
    'Who reversed an unsubscribe (the person, or an operator with a reason) and when, with what the record said. No address; Account erasure replaces the actor.';

RESET lock_timeout;
