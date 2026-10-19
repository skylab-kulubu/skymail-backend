-- The receipts of open and click tracking's retention run (hub ticket
-- iletisim-servisi 14c; design note 07 §4.7; README, "Saklama"). One row per
-- run of SKYMAIL_TRACKING_RETENTION's dry-run, apply or purge: when, how it
-- ended, and what its rules counted (a dry run) or changed. Only counts and
-- times: no person, token, address or link is ever in it. The schedule reads
-- the last run from here, so a restart or a release never runs it twice in a
-- day, and a dry run's counts can be read back before apply is turned on.
--
-- A new table only: an image from before neither reads nor writes it
-- (docs/database-migrations.md, expand).
SET lock_timeout = '5s';

CREATE TABLE tracking_retention_runs
(
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    mode        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'running',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    -- Mail sent at or before this has no events left once apply ran.
    events_cutoff TIMESTAMPTZ NOT NULL,
    counts      JSONB       NOT NULL DEFAULT '{}',
    CONSTRAINT tracking_retention_runs_mode CHECK (mode IN ('dry-run', 'apply', 'purge')),
    CONSTRAINT tracking_retention_runs_status CHECK (status IN ('running', 'ok', 'partial', 'refused_large', 'failed')),
    CONSTRAINT tracking_retention_runs_finished CHECK ((status = 'running') = (finished_at IS NULL))
);

CREATE INDEX idx_tracking_retention_runs_mode ON tracking_retention_runs (mode, started_at);

COMMENT ON TABLE tracking_retention_runs IS
    'Receipts of the open and click tracking retention run: mode, times, status, counts. No person, token or link.';

RESET lock_timeout;
