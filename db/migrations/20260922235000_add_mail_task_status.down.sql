DROP INDEX IF EXISTS idx_mail_tasks_created_at_id;
DROP INDEX IF EXISTS idx_mail_queue_failed_created_at;
DROP INDEX IF EXISTS idx_mail_queue_sent_created_at;
DROP INDEX IF EXISTS idx_mail_queue_task_id_status;
DROP FUNCTION IF EXISTS mail_task_status(UUID);
