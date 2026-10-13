-- What a send's content panel may show, and how long the content is kept.
--
-- Every queue row already holds its recipient's mail as it was rendered and
-- sent (subject, body, body_html); until now it was kept with no end date and
-- shown nowhere. Showing it needs two things this migration adds:
--
-- templates.content_sensitive: whether the content of a send of this template
--   is never shown. true for mail that carries a code or a one-time link (a
--   login or verification code, a password reset, an account confirmation);
--   false for mail whose content may be shown; NULL, the default, leaves it to
--   the kind of send: a single mail (POST /mail_tasks/single, the mail a
--   service sends one person) is hidden, a send to a list, a group or an
--   approved request is shown. The Template seed sets it per template key.
--
-- mail_tasks.content_cleared_at: when the content sweep emptied the send's
--   content: its rows' subject, body and body_html, and its body_variables. A
--   sensitive send is emptied as soon as none of its rows is still to go out,
--   any other MAIL_CONTENT_RETENTION_DAYS (90 by default) after it was
--   created. NULL is a send whose content is still kept.
--
-- Both columns are nullable without a default: adding them rewrites nothing,
-- and an image from before them neither reads nor writes them (sqlc names its
-- columns). That image does not sweep; the content it queues is swept once the
-- new image runs.
--
-- lock_timeout as in 20261012120000: each ALTER holds its table's ACCESS
-- EXCLUSIVE lock for an instant but waits behind any transaction holding the
-- table; past 5 seconds the migration fails, rolled back, and the old task
-- keeps sending (docs/database-migrations.md).
SET lock_timeout = '5s';

ALTER TABLE templates
    ADD COLUMN content_sensitive BOOLEAN;

ALTER TABLE mail_tasks
    ADD COLUMN content_cleared_at TIMESTAMPTZ;

COMMENT ON COLUMN templates.content_sensitive IS
    'Whether the content of a send of this template is never shown: true for mail carrying a code or a one-time link, false for mail that may be shown, null to leave it to the kind of send (a single mail hidden, a send to many shown). Set by the Template seed.';

-- The templates that carry a code or a one-time link, by the keys the
-- services send them by, so they are hidden on any kind of send before the
-- next seed states it: Keycloak's mails (verification and reset links, the
-- personal e-mail code), the account center's (cancel and secure-account
-- links, a device's address), core's contact consent confirmation link and
-- an event ticket's code and QR link.
UPDATE templates
SET content_sensitive = true
WHERE key LIKE 'keycloak.%'
   OR key LIKE 'account.%'
   OR key IN ('core.contact-consent-confirm', 'event.ticket-created', 'event.reminder');

-- And those whose content may be shown, as the seed states them (skymail-
-- frontend emails/): left NULL until the next seed, their single mails would
-- count as sensitive and the first sweep would empty them for good. A value an
-- operator or a seed already wrote is kept.
UPDATE templates
SET content_sensitive = false
WHERE content_sensitive IS NULL
  AND key IN ('free.basic', 'core.welcome', 'core.certificate', 'club.team-membership', 'event.updated',
              'event.cancelled', 'mail.approval-requested', 'mail.approval-resolved', 'ops.send-failed');

-- Every change of a template's content_sensitive, by the panel or the seed:
-- who, when, the value before and after. Never rewritten but by Account
-- erasure, which replaces the actor with Silinmiş kullanıcı.
CREATE TABLE template_content_sensitive_events
(
    id          UUID PRIMARY KEY              DEFAULT gen_random_uuid(),
    template_id UUID                 NOT NULL REFERENCES templates (id),
    actor_kind  template_author_kind NOT NULL,
    actor_sub   TEXT,
    actor_name  TEXT,
    old_value   BOOLEAN,
    new_value   BOOLEAN,
    created_at  TIMESTAMPTZ          NOT NULL DEFAULT now(),
    CONSTRAINT template_content_sensitive_events_changed CHECK (old_value IS DISTINCT FROM new_value)
);

CREATE INDEX idx_template_content_sensitive_events_template
    ON template_content_sensitive_events (template_id, created_at DESC, id DESC);
CREATE INDEX idx_template_content_sensitive_events_actor
    ON template_content_sensitive_events (actor_sub);

-- A send to a list or a Keycloak group, or one an approved Mail onayı request
-- queued: what the club writes to many. Anything else is a single mail. The
-- one definition, read by mail_task_content_sensitive() and the API.
CREATE FUNCTION mail_task_bulk(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT EXISTS (SELECT 1 FROM mail_tasks WHERE id = for_task AND mail_list_id IS NOT NULL)
           OR EXISTS (SELECT 1 FROM mail_approval_tasks WHERE task_id = for_task)
$$;

-- Whether a send's content is never shown: its template says so, or, when the
-- template leaves it to the send (NULL, or no template), it is a single mail.
-- A task that does not exist is sensitive: nothing is shown by mistake.
CREATE FUNCTION mail_task_content_sensitive(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT COALESCE((SELECT t.content_sensitive
                 FROM mail_tasks mt
                          JOIN templates t ON t.id = mt.template_id
                 WHERE mt.id = for_task),
                NOT mail_task_bulk(for_task))
$$;

RESET lock_timeout;
