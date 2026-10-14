SET lock_timeout = '5s';

ALTER TABLE templates
    DROP COLUMN IF EXISTS sensitive_variables;

ALTER TABLE mail_tasks
    DROP COLUMN IF EXISTS content_mask_failing_since;

RESET lock_timeout;
