-- Who read a person's mail history (hub ticket skymail-yeni-tasarim B2;
-- GET /v1/recipients/history): every read is written before it is answered,
-- as an opening of masked content is (sent_content_views).
--
--   address_sha256 SHA-256 (hex) of the address read, trimmed and
--                  lower-cased: the record tells whose history was read to
--                  anyone who knows the address, and holds no address itself.
--   actor_sub      who read it, as their token named them.
--   actor_name     the name their token carried then.
--   results        how many of the address's rows the answer counted.
--
-- Kept SENT_CONTENT_VIEWS_RETENTION_DAYS, then the content sweep deletes it.
-- Account erasure deletes the reads of the person's addresses and replaces
-- the reader with Silinmiş kullanıcı (docs/data-lifecycle.md).
--
-- A new table only: an image from before neither reads nor writes it
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE recipient_history_views
(
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    address_sha256 TEXT        NOT NULL,
    actor_sub      TEXT,
    actor_name     TEXT,
    results        INTEGER     NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT recipient_history_views_address CHECK (address_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT recipient_history_views_results CHECK (results >= 0)
);

COMMENT ON TABLE recipient_history_views IS
    'Each read of an address''s mail history: whose (SHA-256 of the address), who, when. Account erasure deletes the person''s and replaces the reader.';

CREATE INDEX idx_recipient_history_views_address ON recipient_history_views (address_sha256);
CREATE INDEX idx_recipient_history_views_created ON recipient_history_views (created_at);
CREATE INDEX idx_recipient_history_views_actor ON recipient_history_views (actor_sub);

RESET lock_timeout;
