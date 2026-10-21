SET lock_timeout = '5s';

DROP TABLE IF EXISTS inbox_items;
DROP TABLE IF EXISTS notification_variants;
-- A type that went only to the inbox is taken out of use rather than deleted
-- (notifications name it); it keeps a channel the check allows.
UPDATE notification_types
SET default_channels = CASE
                           WHEN default_channels = ARRAY ['inbox']::text[] THEN ARRAY ['email']::text[]
                           ELSE array_remove(default_channels, 'inbox') END,
    archived_at      = CASE
                           WHEN default_channels = ARRAY ['inbox']::text[] THEN COALESCE(archived_at, now())
                           ELSE archived_at END
WHERE 'inbox' = ANY (default_channels);
ALTER TABLE notification_types
    DROP CONSTRAINT notification_types_channels,
    ADD CONSTRAINT notification_types_channels CHECK (cardinality(default_channels) >= 1
        AND default_channels <@ ARRAY ['email']::text[]);

RESET lock_timeout;
