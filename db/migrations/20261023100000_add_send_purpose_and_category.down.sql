SET lock_timeout = '5s';

ALTER TABLE mail_approvals
    DROP CONSTRAINT IF EXISTS mail_approvals_purpose_fields_check,
    DROP CONSTRAINT IF EXISTS mail_approvals_category_check,
    DROP CONSTRAINT IF EXISTS mail_approvals_purpose_check,
    DROP COLUMN IF EXISTS commercial_content_absent,
    DROP COLUMN IF EXISTS transactional_statement,
    DROP COLUMN IF EXISTS category,
    DROP COLUMN IF EXISTS purpose;

ALTER TABLE mail_tasks
    DROP CONSTRAINT IF EXISTS mail_tasks_purpose_fields_check,
    DROP CONSTRAINT IF EXISTS mail_tasks_category_check,
    DROP CONSTRAINT IF EXISTS mail_tasks_purpose_check,
    DROP COLUMN IF EXISTS commercial_content_absent,
    DROP COLUMN IF EXISTS transactional_statement,
    DROP COLUMN IF EXISTS category,
    DROP COLUMN IF EXISTS purpose;

RESET lock_timeout;
