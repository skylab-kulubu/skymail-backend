-- Approval requests and their history go; the sends approved ones queued stay
-- in mail_tasks like any other send.
DROP TABLE IF EXISTS mail_approval_events;
DROP TYPE IF EXISTS mail_approval_event_kind;
DROP TABLE IF EXISTS mail_approvals;
DROP TYPE IF EXISTS mail_approval_state;
