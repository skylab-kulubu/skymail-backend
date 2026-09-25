-- Mail onayı to several people (Yusuf, 2026-09-24; ticket 21): a request goes
-- to a mailing list, as before, or to 1..100 people. The approver decides it
-- once; approving it queues one send per person, as POST /mail_tasks/single
-- sends to one, all in the approval's transaction.
--
-- The one recipient a request could hold moves to mail_approval_recipients,
-- and its one send to mail_approval_tasks. A binary older than this one reads
-- and writes the columns dropped here, so between this migration and the new
-- binary taking over, its approval routes fail — each in its own transaction,
-- so nothing is half done.

-- The people a request goes to, in the order they were submitted. An address
-- is there once, whatever its case. Its name may be empty: a send to a person
-- needs none.
CREATE TABLE mail_approval_recipients
(
    approval_id UUID NOT NULL REFERENCES mail_approvals (id),
    -- 1, 2, 3… in the order submitted; the first is who the preview is for.
    position    INT  NOT NULL,
    email       TEXT NOT NULL,
    full_name   TEXT NOT NULL,
    PRIMARY KEY (approval_id, position),
    CONSTRAINT mail_approval_recipients_position CHECK (position BETWEEN 1 AND 100),
    CONSTRAINT mail_approval_recipients_email CHECK (btrim(email) <> '')
);
CREATE UNIQUE INDEX mail_approval_recipients_email_once ON mail_approval_recipients (approval_id, lower(email));

INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
SELECT id, 1, recipient_email, COALESCE(recipient_full_name, '')
FROM mail_approvals
WHERE recipient_email IS NOT NULL;

-- The sends an approved request queued: the list's one send, or one per
-- person, at that person's position.
CREATE TABLE mail_approval_tasks
(
    approval_id UUID NOT NULL REFERENCES mail_approvals (id),
    position    INT  NOT NULL CHECK (position > 0),
    task_id     UUID NOT NULL UNIQUE REFERENCES mail_tasks (id),
    PRIMARY KEY (approval_id, position)
);

INSERT INTO mail_approval_tasks (approval_id, position, task_id)
SELECT id, 1, task_id
FROM mail_approvals
WHERE task_id IS NOT NULL;

ALTER TABLE mail_approvals
    DROP CONSTRAINT mail_approvals_one_audience,
    DROP CONSTRAINT mail_approvals_recipient_name,
    DROP CONSTRAINT mail_approvals_task_only_when_approved,
    DROP COLUMN recipient_email,
    DROP COLUMN recipient_full_name,
    DROP COLUMN task_id;

-- What the dropped checks held, now across the three tables, checked when the
-- transaction commits so a request and its people or sends can be written in
-- turn:
--
--   mail_approvals_one_audience         a list or at least one person, never
--                                       both;
--   mail_approvals_sends_when_approved  sends only on an approved request.
CREATE FUNCTION check_mail_approval() RETURNS trigger
    LANGUAGE plpgsql AS
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

CREATE CONSTRAINT TRIGGER check_mail_approval
    AFTER INSERT OR UPDATE ON mail_approvals
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
EXECUTE FUNCTION check_mail_approval();

CREATE CONSTRAINT TRIGGER check_mail_approval
    AFTER INSERT OR UPDATE OR DELETE ON mail_approval_recipients
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
EXECUTE FUNCTION check_mail_approval();

CREATE CONSTRAINT TRIGGER check_mail_approval
    AFTER INSERT OR UPDATE ON mail_approval_tasks
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
EXECUTE FUNCTION check_mail_approval();
