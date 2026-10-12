-- The send budget's counters (SKYMAIL_RATE_PER_MINUTE,
-- SKYMAIL_DAILY_RECIPIENT_BUDGET): how many queue rows the dispatchers of all
-- processes took in a window of span_seconds (60, a minute; 86400, a day)
-- starting at starts_at, by lane (mail_queue.priority > 0: 1 the system lane,
-- 0 the bulk lane). Every process counts here, so the budget holds across
-- replicas. A process takes a bulk row only under the advisory lock
-- 'skymail-backend send budget' and only while the windows leave room; a
-- system row is always taken and counted after. Old windows are pruned.
--
-- A new table: an image from before it neither reads nor writes it and sends
-- as it always has. lock_timeout as in 20261009120000, though nothing else
-- can hold a table that does not exist yet.
SET lock_timeout = '5s';

CREATE TABLE mail_send_windows
(
    span_seconds INT         NOT NULL CHECK (span_seconds > 0),
    starts_at    TIMESTAMPTZ NOT NULL,
    lane         SMALLINT    NOT NULL CHECK (lane IN (0, 1)),
    taken        INT         NOT NULL CHECK (taken >= 0),
    PRIMARY KEY (span_seconds, starts_at, lane)
);

RESET lock_timeout;
