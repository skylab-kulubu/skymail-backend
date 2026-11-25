-- A send's own events (hub ticket skymail-yeni-tasarim B3; GET
-- /v1/mail_tasks/{id}/events): what mail_tasks keeps only as its current
-- state. A pause, a resume and a cancel are written by whoever did them; a
-- send that paused itself (failure_rate) and the mailer's retries are written
-- by SkyMail (no actor). Everything else the endpoint tells — created, the
-- Mail onayı events, queued, finished, permanent failures — is read from the
-- rows that already hold it.
--
--   kind       paused, resumed, cancelled or retried.
--   reason     a pause's: operator or failure_rate; null otherwise.
--   actor_sub  who did it, as their token named them; null for SkyMail.
--   actor_name the name their token carried then.
--   count      cancelled: the pending recipients the cancel skipped;
--              retried: the retries scheduled in the hour; null otherwise.
--   hour       retried only: the hour (UTC) its retries fell in. One row a
--              send and hour, counted up, so a send whose relay fails for a
--              day holds at most 24 such rows, not one a retry.
--   created_at when it happened (retried: the hour's first retry).
--   last_at    retried: the hour's last retry; created_at otherwise.
--
-- Account erasure replaces actor_sub and actor_name with Silinmiş kullanıcı
-- (docs/data-lifecycle.md). A send that is deleted takes its events with it.
--
-- A new table only: an image from before neither reads nor writes it
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE mail_task_events
(
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    UUID        NOT NULL REFERENCES mail_tasks (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL,
    reason     TEXT,
    actor_sub  TEXT,
    actor_name TEXT,
    count      INTEGER,
    hour       TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT mail_task_events_kind CHECK (kind IN ('paused', 'resumed', 'cancelled', 'retried')),
    CONSTRAINT mail_task_events_reason CHECK ((kind = 'paused') = (reason IS NOT NULL)
        AND (reason IS NULL OR reason IN ('operator', 'failure_rate'))),
    CONSTRAINT mail_task_events_hour CHECK ((kind = 'retried') = (hour IS NOT NULL)),
    CONSTRAINT mail_task_events_count CHECK (count IS NULL OR count >= 0)
);

COMMENT ON TABLE mail_task_events IS
    'A send''s pauses, resumes, cancels (who) and retries (counted by the hour); GET /v1/mail_tasks/{id}/events.';

CREATE INDEX idx_mail_task_events_task ON mail_task_events (task_id, created_at, id);

-- One retried row a send and hour (RescheduleMailQueueItem counts it up).
CREATE UNIQUE INDEX idx_mail_task_events_retry_hour ON mail_task_events (task_id, hour) WHERE kind = 'retried';

RESET lock_timeout;
