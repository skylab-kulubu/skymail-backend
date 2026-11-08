SET lock_timeout = '5s';

ALTER TABLE mail_approvals
    DROP CONSTRAINT mail_approvals_team_ids,
    DROP COLUMN team_ids;

RESET lock_timeout;
