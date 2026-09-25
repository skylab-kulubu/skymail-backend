-- Back to one recipient and one send per request. A request that goes to
-- several people cannot be put back on one row without dropping people, so
-- down refuses while there is one — before it changes anything — and says
-- which. The file runs as one transaction: refused, it leaves the schema as
-- it was, and golang-migrate marks the version dirty until an operator runs
-- `migrate force 20260925100000`.
DO
$$
DECLARE
    several BIGINT;
    example UUID;
BEGIN
    SELECT count(*), min(approval_id::text)::uuid
    INTO several, example
    FROM (SELECT approval_id
          FROM mail_approval_recipients
          GROUP BY approval_id
          HAVING count(*) > 1
          UNION
          SELECT approval_id
          FROM mail_approval_tasks
          GROUP BY approval_id
          HAVING count(*) > 1) s;
    IF several > 0 THEN
        RAISE EXCEPTION '% Mail onayı request(s) go to more than one person (one is %); the schema before 20260925100000 holds one recipient per request',
            several, example
            USING HINT = 'Nothing was changed. Decide what to do with those requests first, then run `migrate force 20260925100000` and down again.';
    END IF;
END
$$;

DROP TRIGGER check_mail_approval ON mail_approval_tasks;
DROP TRIGGER check_mail_approval ON mail_approval_recipients;
DROP TRIGGER check_mail_approval ON mail_approvals;
DROP FUNCTION check_mail_approval();

ALTER TABLE mail_approvals
    ADD COLUMN recipient_email     TEXT,
    ADD COLUMN recipient_full_name TEXT,
    ADD COLUMN task_id             UUID REFERENCES mail_tasks (id);

UPDATE mail_approvals a
SET recipient_email     = r.email,
    recipient_full_name = r.full_name
FROM mail_approval_recipients r
WHERE r.approval_id = a.id;

UPDATE mail_approvals a
SET task_id = t.task_id
FROM mail_approval_tasks t
WHERE t.approval_id = a.id;

ALTER TABLE mail_approvals
    ADD CONSTRAINT mail_approvals_one_audience CHECK ((mail_list_id IS NULL) <> (recipient_email IS NULL)),
    ADD CONSTRAINT mail_approvals_recipient_name CHECK (recipient_email IS NOT NULL OR recipient_full_name IS NULL),
    ADD CONSTRAINT mail_approvals_task_only_when_approved CHECK (task_id IS NULL OR state = 'approved');

DROP TABLE mail_approval_tasks;
DROP TABLE mail_approval_recipients;
