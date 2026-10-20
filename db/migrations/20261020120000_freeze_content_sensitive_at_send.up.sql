-- A send's sensitivity is decided when it is queued, for good (hub ticket
-- iletisim-servisi 21; #61 review D1, B3/D5, D3/D6).
--
-- Until now mail_tasks.content_sensitive_at_send copied the template's
-- content_sensitive alone, and whether the template's key is locked
-- (template_key_content_locked) was read from the template's key as it is
-- now. A locked template whose key changed (PATCH refuses it since #61, but
-- by hand or by any later path) took the lock off its old sends with it: a
-- send with a code queued while the key was locked and the row said false
-- or nothing read as shown. From this migration on the copy is written
-- 'true' when the key is locked when the send is queued
-- (CreateMailTask, CreateSingleMailTask, InsertMailTask), so the send stays
-- sensitive whatever happens to its template afterwards.
--
-- The sends already kept are filled once with what decides today
-- (mail_task_content_sensitive(): the lock, the template's value now or at
-- send, a single mail left to its kind): each send sensitive now is written
-- 'true'. A send not sensitive now keeps its copy. Only rows are updated:
-- no table lock beyond the rows (lock_timeout as in 20261012180000 for the
-- function).
--
-- mail_task_content_sensitive() reads the copy first: a send written 'true'
-- is sensitive without looking further. The lock on the key as it is now
-- and the template's value now stay behind it, a defence for the sends an
-- image from before this one queues during a rollout (it writes the
-- template's value alone) and for a template made sensitive after a send.
--
-- Rolling back to an image from before this one: it reads the same column
-- the same way ('true' is sensitive), so the frozen sends stay sensitive.
-- Do not run the down migration: it gives back 20261013140000's function
-- and leaves the copies as they are.
SET lock_timeout = '5s';

CREATE OR REPLACE FUNCTION mail_task_content_sensitive(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT COALESCE((SELECT CASE
                            WHEN mt.content_sensitive_at_send = 'true' THEN true
                            ELSE template_key_content_locked(t.key)
                                OR COALESCE(t.content_sensitive, NOT mail_task_bulk(mt.id))
                                OR (mt.content_sensitive_at_send = 'unset' AND NOT mail_task_bulk(mt.id))
                            END
                 FROM mail_tasks mt
                          LEFT JOIN templates t ON t.id = mt.template_id
                 WHERE mt.id = for_task),
                true)
$$;

COMMENT ON COLUMN mail_tasks.content_sensitive_at_send IS
    'Whether the send was sensitive when it was queued: true when its template said so or its key was locked sensitive (template_key_content_locked), false when its template said it may be shown, unset when the template said nothing. A send is sensitive if this or its template now says so.';

UPDATE mail_tasks
SET content_sensitive_at_send = 'true'
WHERE content_sensitive_at_send <> 'true'
  AND mail_task_content_sensitive(id);

RESET lock_timeout;
