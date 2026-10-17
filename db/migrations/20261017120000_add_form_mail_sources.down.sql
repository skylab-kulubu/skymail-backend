-- A rollback must not silently destroy a durable snapshot or make its
-- approvals unreadable. Revert only while no Forms sources have been used.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM mail_form_sources) THEN
  RAISE EXCEPTION 'Forms snapshots exist; use a compatible binary instead of destructive rollback';
 END IF;
END $$;
ALTER TABLE mail_tasks DROP CONSTRAINT mail_tasks_one_source;
ALTER TABLE mail_tasks DROP COLUMN form_source_id;
ALTER TABLE mail_approvals DROP COLUMN form_source_id;
DROP TABLE mail_form_source_recipients;
DROP TABLE mail_form_sources;
DROP FUNCTION check_mail_form_source();
CREATE OR REPLACE FUNCTION mail_task_bulk(for_task UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path = pg_catalog, public AS $$
SELECT EXISTS (SELECT 1 FROM mail_tasks WHERE id = for_task AND mail_list_id IS NOT NULL)
 OR EXISTS (SELECT 1 FROM mail_approval_tasks WHERE task_id = for_task)
$$;
CREATE OR REPLACE FUNCTION check_mail_approval() RETURNS trigger
    LANGUAGE plpgsql SET search_path = pg_catalog, public AS
$$
DECLARE
    requests  UUID[];
    request   UUID;
    to_list   BOOLEAN;
    to_people BOOLEAN;
    state_now mail_approval_state;
    has_sends BOOLEAN;
BEGIN
    -- The request the row is of; a person or a send moved between requests
    -- changes both.
    IF TG_TABLE_NAME = 'mail_approvals' THEN
        requests := ARRAY [NEW.id];
    ELSIF TG_OP = 'INSERT' THEN
        requests := ARRAY [NEW.approval_id];
    ELSIF TG_OP = 'DELETE' THEN
        requests := ARRAY [OLD.approval_id];
    ELSE
        requests := ARRAY [OLD.approval_id, NEW.approval_id];
    END IF;

    FOREACH request IN ARRAY requests
        LOOP
            SELECT a.mail_list_id IS NOT NULL,
                   EXISTS (SELECT 1 FROM mail_approval_recipients r WHERE r.approval_id = a.id),
                   a.state,
                   EXISTS (SELECT 1 FROM mail_approval_tasks t WHERE t.approval_id = a.id)
            INTO to_list, to_people, state_now, has_sends
            FROM mail_approvals a
            WHERE a.id = request;
            CONTINUE WHEN NOT FOUND;

            IF to_list = to_people THEN
                RAISE EXCEPTION 'mail approval % goes to %', request,
                    CASE WHEN to_list THEN 'a mailing list and to people' ELSE 'no one' END
                    USING ERRCODE = 'check_violation', CONSTRAINT = 'mail_approvals_one_audience', TABLE = 'mail_approvals';
            END IF;
            IF has_sends AND state_now <> 'approved' THEN
                RAISE EXCEPTION 'mail approval % has sends but is %', request, state_now
                    USING ERRCODE = 'check_violation', CONSTRAINT = 'mail_approvals_sends_when_approved', TABLE = 'mail_approvals';
            END IF;
        END LOOP;
    RETURN NULL;
END
$$;
