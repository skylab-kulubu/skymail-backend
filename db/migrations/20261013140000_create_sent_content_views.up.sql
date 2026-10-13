-- Who opened a sensitive send's content (Yusuf, 2026-10-13: "içerik özel de
-- olsa görebilelim", codes and one-time links masked): a sensitive send's
-- content is kept now, as any other's, and shown with every code and one-time
-- link masked on the server. Each time someone opens one recipient's masked
-- mail (GET /mail_tasks/{id}/queue/{itemId}/content), a row here says who,
-- which send and which recipient's row, when, and what was masked.
--
-- queue_item_id is not a foreign key: Account erasure deletes a pending row
-- to the person, and the record that someone opened it stays. The record
-- names no recipient; the row it names is emptied or deleted with the person.
-- Account erasure replaces actor_sub and actor_name with Silinmiş kullanıcı,
-- as it does in template_content_sensitive_events.
--
-- A new, empty table: nothing older reads or writes it, and creating it takes
-- no lock another statement waits on but mail_tasks' for the foreign key, for
-- an instant (lock_timeout as in 20261012180000).
SET lock_timeout = '5s';

CREATE TABLE sent_content_views
(
    id               UUID PRIMARY KEY     DEFAULT gen_random_uuid(),
    task_id          UUID        NOT NULL REFERENCES mail_tasks (id),
    queue_item_id    UUID        NOT NULL,
    -- The Keycloak subject of the token that opened it, and the name it
    -- carried then.
    actor_sub        TEXT,
    actor_name       TEXT,
    -- variables: rendered again with the code and link variables masked;
    -- patterns: the stored mail with every link, long number and code masked.
    masked_by        TEXT        NOT NULL CHECK (masked_by IN ('variables', 'patterns')),
    -- The variables masked, by their path in the send's variables.
    masked_variables TEXT[]      NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sent_content_views_task ON sent_content_views (task_id, created_at DESC);
CREATE INDEX idx_sent_content_views_actor ON sent_content_views (actor_sub);

COMMENT ON TABLE sent_content_views IS
    'Each opening of a sensitive send''s masked content: who, which send and queue row, when, and what was masked. Account erasure replaces the actor.';

RESET lock_timeout;
