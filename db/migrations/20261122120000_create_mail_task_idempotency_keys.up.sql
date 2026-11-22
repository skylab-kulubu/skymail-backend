-- A service's Idempotency-Key on POST /v1/mail_tasks/single (hub tickets
-- forms-posta-core 11 and horizontal-scale 28): the caller's key, a SHA-256
-- of the request as it was understood, the send it queued and the answer it
-- got. A retry with the same key answers that answer again and queues
-- nothing; another request with it is refused. The row is written in the
-- transaction that queues the send, so the send and its key exist together
-- or not at all.
--
-- sent_by is the service account's Keycloak subject (only a service's token
-- may send a key), never a person's. No address: the request is kept as its
-- digest only. Kept 72 hours (IdempotencyKeyRetention), then the content
-- sweep deletes the row; a send that is deleted takes its key with it.
--
-- A new table only: an image from before neither reads nor writes it
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_task_idempotency_keys
(
    sent_by         TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    request_sha256  TEXT        NOT NULL,
    task_id         UUID        NOT NULL REFERENCES mail_tasks (id) ON DELETE CASCADE,
    result          JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sent_by, idempotency_key),
    CONSTRAINT mail_task_idempotency_keys_key CHECK (idempotency_key ~ '^[!-~]{1,255}$'),
    CONSTRAINT mail_task_idempotency_keys_sha256 CHECK (request_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX idx_mail_task_idempotency_keys_task ON mail_task_idempotency_keys (task_id);
CREATE INDEX idx_mail_task_idempotency_keys_created ON mail_task_idempotency_keys (created_at);

COMMENT ON TABLE mail_task_idempotency_keys IS
    'A service''s Idempotency-Key on POST /mail_tasks/single: the request''s digest, the send it queued and its answer, replayed to a retry. No address. Kept 72 hours.';
COMMENT ON COLUMN mail_task_idempotency_keys.request_sha256 IS
    'SHA-256 (hex) of the request as it was understood: the same key with another request is refused.';
COMMENT ON COLUMN mail_task_idempotency_keys.result IS
    'The 201 answer the request got (the send''s id, tracking, purpose): a retry gets it again.';

RESET lock_timeout;
