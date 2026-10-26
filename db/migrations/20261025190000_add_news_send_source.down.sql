SET lock_timeout = '5s';

ALTER TABLE mail_queue
    DROP CONSTRAINT IF EXISTS mail_queue_source_ref_check,
    DROP COLUMN IF EXISTS source_ref;

ALTER TABLE mail_tasks
    DROP CONSTRAINT IF EXISTS mail_tasks_source_check,
    DROP COLUMN IF EXISTS source_counts,
    DROP COLUMN IF EXISTS source_audience,
    DROP COLUMN IF EXISTS source_snapshot,
    DROP COLUMN IF EXISTS source_version,
    DROP COLUMN IF EXISTS source_ref,
    DROP COLUMN IF EXISTS source_kind;

RESET lock_timeout;
