-- Forms owns permission checks and resolution. SkyMail owns this immutable
-- recipient snapshot and its single use by one submitter. Existing named
-- recipients keep their own table and its position BETWEEN 1 AND 100 check.
CREATE TABLE mail_form_sources (
    id UUID PRIMARY KEY,
    owner_sub TEXT NOT NULL CHECK (btrim(owner_sub) <> ''),
    form_id UUID NOT NULL,
    form_title TEXT NOT NULL CHECK (char_length(form_title) BETWEEN 1 AND 100),
    prepared_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > prepared_at),
    response_count INT NOT NULL CHECK (response_count >= 0),
    recipient_count INT NOT NULL CHECK (recipient_count > 0),
    skipped_count INT NOT NULL CHECK (skipped_count >= 0),
    duplicate_count INT NOT NULL CHECK (duplicate_count >= 0),
    snapshot_hash BYTEA NOT NULL CHECK (octet_length(snapshot_hash) = 32),
    invalidated_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    consumed_approval_id UUID REFERENCES mail_approvals(id) DEFERRABLE INITIALLY DEFERRED,
    consumed_task_id UUID REFERENCES mail_tasks(id) DEFERRABLE INITIALLY DEFERRED,
    consume_request_hash BYTEA,
    CONSTRAINT mail_form_sources_counts CHECK
        (response_count::bigint = recipient_count::bigint + skipped_count::bigint + duplicate_count::bigint),
    CONSTRAINT mail_form_sources_consumed CHECK (
        (consumed_at IS NULL AND consumed_approval_id IS NULL AND consumed_task_id IS NULL AND consume_request_hash IS NULL)
        OR (consumed_at IS NOT NULL AND num_nonnulls(consumed_approval_id, consumed_task_id) = 1
            AND consume_request_hash IS NOT NULL AND octet_length(consume_request_hash) = 32))
);
CREATE INDEX mail_form_sources_expiry ON mail_form_sources(expires_at, id) WHERE consumed_at IS NULL;
CREATE TABLE mail_form_source_recipients (
    source_id UUID NOT NULL REFERENCES mail_form_sources(id) ON DELETE CASCADE,
    position INT NOT NULL CHECK (position > 0),
    email TEXT NOT NULL CHECK (btrim(email) <> '' AND char_length(email) <= 254),
    full_name TEXT NOT NULL,
    PRIMARY KEY(source_id, position)
);
CREATE UNIQUE INDEX mail_form_source_recipients_email_once ON mail_form_source_recipients(source_id, lower(email));
ALTER TABLE mail_approvals ADD COLUMN form_source_id UUID REFERENCES mail_form_sources(id);
ALTER TABLE mail_tasks ADD COLUMN form_source_id UUID REFERENCES mail_form_sources(id);
CREATE UNIQUE INDEX mail_approvals_form_source_once ON mail_approvals(form_source_id) WHERE form_source_id IS NOT NULL;
CREATE UNIQUE INDEX mail_tasks_form_source_once ON mail_tasks(form_source_id) WHERE form_source_id IS NOT NULL;
ALTER TABLE mail_tasks ADD CONSTRAINT mail_tasks_one_source CHECK (mail_list_id IS NULL OR form_source_id IS NULL);

CREATE FUNCTION check_mail_form_source() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, public AS $$
DECLARE source UUID; expected INT; invalidated TIMESTAMPTZ; actual BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'mail_form_sources' THEN source := NEW.id;
    ELSIF TG_OP = 'DELETE' THEN source := OLD.source_id;
    ELSE source := NEW.source_id; END IF;
    SELECT recipient_count, invalidated_at INTO expected, invalidated FROM mail_form_sources WHERE id = source;
    IF NOT FOUND OR invalidated IS NOT NULL THEN RETURN NULL; END IF;
    SELECT count(*) INTO actual FROM mail_form_source_recipients WHERE source_id = source;
    IF actual <> expected THEN
        RAISE EXCEPTION 'Forms snapshot count changed' USING ERRCODE = 'check_violation', CONSTRAINT = 'mail_form_sources_snapshot_count';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER check_mail_form_source AFTER INSERT OR UPDATE ON mail_form_sources
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_mail_form_source();
CREATE CONSTRAINT TRIGGER check_mail_form_source AFTER INSERT OR UPDATE OR DELETE ON mail_form_source_recipients
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_mail_form_source();

CREATE OR REPLACE FUNCTION mail_task_bulk(for_task UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE SET search_path = pg_catalog, public AS $$
SELECT EXISTS (SELECT 1 FROM mail_tasks WHERE id = for_task AND (mail_list_id IS NOT NULL OR form_source_id IS NOT NULL))
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
    source_id UUID;
    source_owner TEXT;
    source_approval UUID;
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
                   a.form_source_id,
                   EXISTS (SELECT 1 FROM mail_approval_tasks t WHERE t.approval_id = a.id)
            INTO to_list, to_people, state_now, source_id, has_sends
            FROM mail_approvals a
            WHERE a.id = request;
            CONTINUE WHEN NOT FOUND;

            IF source_id IS NOT NULL THEN
                SELECT owner_sub, consumed_approval_id INTO source_owner, source_approval
                FROM mail_form_sources WHERE id = source_id;
                IF to_list OR to_people OR source_approval IS DISTINCT FROM request
                    OR source_owner IS DISTINCT FROM (SELECT submitter_sub FROM mail_approvals WHERE id = request) THEN
                    RAISE EXCEPTION 'invalid Forms source audience'
                        USING ERRCODE = 'check_violation', CONSTRAINT = 'mail_approvals_one_audience';
                END IF;
            ELSIF to_list = to_people THEN
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
