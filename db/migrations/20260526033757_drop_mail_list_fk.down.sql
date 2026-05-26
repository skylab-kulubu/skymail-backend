ALTER TABLE mail_tasks ADD CONSTRAINT mail_tasks_mail_list_id_fkey FOREIGN KEY (mail_list_id) REFERENCES mailing_lists (id) ON DELETE SET NULL;
