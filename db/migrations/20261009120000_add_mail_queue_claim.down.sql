ALTER TABLE mail_queue
    DROP COLUMN IF EXISTS claimed_by,
    DROP COLUMN IF EXISTS claimed_at;
