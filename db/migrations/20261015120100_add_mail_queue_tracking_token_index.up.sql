-- A tracked row by its token: what the pixel and every click read
-- (/t/o/{token}, /t/c/{token}/{link}). Unique, so a token names one row; only
-- the tracked rows are in it.
--
-- CONCURRENTLY, one statement in its own file, no lock_timeout, and the same
-- recovery from a failed build as 20261012130100 (docs/database-migrations.md,
-- dirty version).
CREATE UNIQUE INDEX CONCURRENTLY idx_mail_queue_tracking_token
    ON mail_queue (tracking_token)
    WHERE tracking_token IS NOT NULL;
