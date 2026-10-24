SET lock_timeout = '5s';

ALTER TABLE mail_approvals
    DROP CONSTRAINT IF EXISTS mail_approvals_own_event_statement_check,
    DROP COLUMN IF EXISTS own_event_statement;

ALTER TABLE mail_tasks
    DROP CONSTRAINT IF EXISTS mail_tasks_own_event_statement_check,
    DROP COLUMN IF EXISTS own_event_statement;

RESET lock_timeout;
