-- Mail onayı (ADR-0031, CONTEXT.md): a send a User without send permission
-- submits, held in SkyMail until an approver — the skymail:mails:approve
-- client role, separate from sending — decides it. It carries what would be
-- sent: a template, pinned to the version published when it was submitted, an
-- audience (a mailing list — internal or a Keycloak group, as mail_tasks keeps
-- it — or one recipient), and the variables. Approving it queues exactly that
-- through the send path; nothing else ever sends it.
--
--   pending   submitted, awaiting an approver;
--   returned  an approver edited it and returned it: the submitter accepts,
--             and it goes out as edited, or declines;
--   approved  sent: queued once, task_id is the send;
--   rejected  an approver refused it, with a reason; the submitter may edit
--             and resubmit it;
--   declined  the submitter refused an approver's edit; they may edit and
--             resubmit it;
--   expired   undecided — pending or returned — at its deadline, seven days
--             after it was last submitted or returned. It never goes out.
--
-- A resubmission is the same request, pending again with a new deadline; the
-- events keep everything that happened before it.
CREATE TYPE mail_approval_state AS ENUM ('pending', 'returned', 'approved', 'rejected', 'declined', 'expired');

CREATE TABLE mail_approvals
(
    id                  UUID PRIMARY KEY             DEFAULT gen_random_uuid(),
    -- The Keycloak subject of the submitter's token, and the name and e-mail
    -- it carried: the decision is mailed there, also when SkyMail itself
    -- expires the request and there is no token to ask.
    submitter_sub       TEXT                NOT NULL,
    submitter_name      TEXT,
    submitter_email     TEXT,
    state               mail_approval_state NOT NULL DEFAULT 'pending',
    -- Templates are archived, never deleted (ADR-0042).
    template_id         UUID                NOT NULL REFERENCES templates (id),
    -- The version the template published when the request was last submitted:
    -- what the submitter filled in and the approver previews. A send must be
    -- of this version, so one the template has since replaced is refused.
    template_version_id UUID                NOT NULL,
    -- An internal mailing list or a Keycloak group, as in mail_tasks, so no
    -- foreign key; or else the one recipient.
    mail_list_id        UUID,
    recipient_email     TEXT,
    recipient_full_name TEXT,
    body_variables      JSONB               NOT NULL,
    created_at          TIMESTAMPTZ         NOT NULL,
    submitted_at        TIMESTAMPTZ         NOT NULL,
    deadline_at         TIMESTAMPTZ         NOT NULL,
    updated_at          TIMESTAMPTZ         NOT NULL,
    -- The send, once approved.
    task_id             UUID REFERENCES mail_tasks (id),
    CONSTRAINT mail_approvals_template_version
        FOREIGN KEY (template_id, template_version_id) REFERENCES template_versions (template_id, id),
    CONSTRAINT mail_approvals_one_audience CHECK ((mail_list_id IS NULL) <> (recipient_email IS NULL)),
    CONSTRAINT mail_approvals_recipient_name CHECK (recipient_email IS NOT NULL OR recipient_full_name IS NULL),
    CONSTRAINT mail_approvals_variables_object CHECK (jsonb_typeof(body_variables) = 'object'),
    CONSTRAINT mail_approvals_submitted_after_created CHECK (submitted_at >= created_at),
    CONSTRAINT mail_approvals_deadline_after_submission CHECK (deadline_at > submitted_at),
    CONSTRAINT mail_approvals_task_only_when_approved CHECK (task_id IS NULL OR state = 'approved')
);

-- The list: everyone's, or one submitter's, newest submission first.
CREATE INDEX idx_mail_approvals_submitted_at_id ON mail_approvals (submitted_at DESC, id DESC);
CREATE INDEX idx_mail_approvals_submitter_submitted_at ON mail_approvals (submitter_sub, submitted_at DESC, id DESC);
-- The expiry sweep reads the undecided requests by deadline.
CREATE INDEX idx_mail_approvals_undecided_deadline ON mail_approvals (deadline_at) WHERE state IN ('pending', 'returned');

-- What happened to a request, in order: who submitted it, who changed what,
-- who decided what and why. Nothing here is ever rewritten.
--
--   submitted, resubmitted  by the submitter; a resubmission's changes are
--                           what it changed;
--   edited                  by an approver, the variables they changed, just
--                           before they approved or returned it;
--   returned                by an approver, to the submitter;
--   accepted, declined      by the submitter, of a returned edit; an
--                           acceptance is the send, like an approval;
--   approved, rejected      by an approver; a rejection's note is its reason;
--   expired                 by SkyMail at the deadline: no actor.
CREATE TYPE mail_approval_event_kind AS ENUM (
    'submitted', 'resubmitted', 'edited', 'returned', 'accepted', 'declined', 'approved', 'rejected', 'expired'
    );

CREATE TABLE mail_approval_events
(
    id          UUID PRIMARY KEY                  DEFAULT gen_random_uuid(),
    approval_id UUID                     NOT NULL REFERENCES mail_approvals (id),
    -- 1, 2, 3… per request, in the order they happened. Writers hold the
    -- request's row lock, so the order is the write order.
    seq         INT                      NOT NULL CHECK (seq > 0),
    kind        mail_approval_event_kind NOT NULL,
    -- Who did it, as their token named them; null for SkyMail itself.
    actor_sub   TEXT,
    actor_name  TEXT,
    -- A rejection's reason, or the note an approver or submitter left.
    note        TEXT,
    -- What an edit or a resubmission changed: [{field, name, before, after}].
    changes     JSONB,
    -- The send an approval or an acceptance queued.
    task_id     UUID REFERENCES mail_tasks (id),
    created_at  TIMESTAMPTZ              NOT NULL,
    CONSTRAINT mail_approval_events_approval_seq UNIQUE (approval_id, seq),
    CONSTRAINT mail_approval_events_actor CHECK ((actor_sub IS NULL) = (kind = 'expired')),
    CONSTRAINT mail_approval_events_rejection_reason CHECK (kind <> 'rejected' OR (note IS NOT NULL AND btrim(note) <> '')),
    CONSTRAINT mail_approval_events_changes_array CHECK (changes IS NULL OR jsonb_typeof(changes) = 'array'),
    CONSTRAINT mail_approval_events_changes_of_edits CHECK (changes IS NULL OR kind IN ('edited', 'resubmitted'))
);
