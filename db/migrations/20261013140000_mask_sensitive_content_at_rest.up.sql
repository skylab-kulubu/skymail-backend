-- A sensitive send's content is kept masked, not raw (Yusuf, 2026-10-13:
-- "içerik özel de olsa görebilelim", codes and one-time links masked; the
-- orchestrator's decision after the #63 review: mask when the content is
-- stored, keep the least). Once none of a sensitive send's rows is still to go
-- out, the content sweep replaces each row's subject, plain text and HTML with
-- the masked copy and the send's body_variables with their masked values: the
-- raw code or link is not kept anywhere but the nightly dump until it rotates.
-- Nothing masks on reading.
--
-- mail_tasks.content_sensitive_at_send: the template's content_sensitive when
--   the send was queued ('true', 'false', or 'unset' for NULL). Whether a send
--   is sensitive is that OR what its template says now: changing a template to
--   false after the send no longer shows its sends unmasked. Rows from before
--   are filled from their template as it is now.
-- mail_tasks.content_masked_at / content_masked_by / content_masked_variables:
--   when the sweep masked the send, how (variables: rendered again with its
--   code and link variables masked, by the template version in force when it
--   was sent; patterns: the stored mail with every link, IP address, long
--   number and code masked) and which variables. A send it could not mask for
--   certain is emptied instead (content_cleared_at), shown as not kept.
-- template_key_content_locked(key): the keys whose mail carries a credential,
--   the same list as internal/handlers/content_sensitivity.go (a test holds
--   the two together). mail_task_content_sensitive() now counts them too, so
--   the sweep masks them whatever their row says.
-- sent_content_views: who opened a sensitive send's masked content
--   (GET /mail_tasks/{id}/queue/{itemId}/content): who, which send and which
--   recipient's row, when. queue_item_id is not a foreign key: Account erasure
--   deletes a pending row to the person, and the record that someone opened it
--   stays. It names no recipient; Account erasure replaces the actor.
--
-- Rolling back to an image from before this one (#61) empties every
-- sensitive send on its first sweep, masked or not: safe, and for good. Do not
-- run the down migration: it drops the record of who opened what.
--
-- An image from before this one does not set content_sensitive_at_send: its
-- sends read 'unset', as if their template said nothing (a single mail is
-- sensitive), and does not mask. lock_timeout as in 20261012180000.
SET lock_timeout = '5s';

ALTER TABLE mail_tasks
    ADD COLUMN content_sensitive_at_send TEXT NOT NULL DEFAULT 'unset'
        CONSTRAINT mail_tasks_content_sensitive_at_send_value CHECK (content_sensitive_at_send IN ('true', 'false', 'unset')),
    ADD COLUMN content_masked_at         TIMESTAMPTZ,
    ADD COLUMN content_masked_by         TEXT
        CONSTRAINT mail_tasks_content_masked_by_value CHECK (content_masked_by IN ('variables', 'patterns')),
    ADD COLUMN content_masked_variables  TEXT[],
    ADD COLUMN content_mask_due          BOOLEAN  NOT NULL DEFAULT true,
    ADD COLUMN content_mask_failures     SMALLINT NOT NULL DEFAULT 0;

UPDATE mail_tasks mt
SET content_sensitive_at_send = t.content_sensitive::text
FROM templates t
WHERE t.id = mt.template_id
  AND t.content_sensitive IS NOT NULL;

COMMENT ON COLUMN mail_tasks.content_sensitive_at_send IS
    'The template''s content_sensitive when the send was queued: true, false or unset (NULL). A send is sensitive if this or its template now says so.';
-- Only the sends still to be looked at are due: a send masked, emptied, or
-- found not sensitive is not, until its template's content_sensitive or key
-- changes (the trigger below). The sweep reads the due ones alone
-- (idx_mail_tasks_content_mask_due, 20261013140100).
UPDATE mail_tasks
SET content_mask_due = false
WHERE content_cleared_at IS NOT NULL;

COMMENT ON COLUMN mail_tasks.content_mask_due IS
    'Whether the content sweep still has to decide on masking the send: true when queued and again when its template''s content_sensitive or key changes; false once masked, emptied or found not sensitive.';
COMMENT ON COLUMN mail_tasks.content_mask_failures IS
    'How many times in a row masking the send failed; at 3 its content is emptied (not kept), so one send cannot hold the others back.';
COMMENT ON COLUMN mail_tasks.content_masked_at IS
    'When the content sweep replaced the send''s content and body_variables with their masked copies; null while raw or never sensitive.';

-- The keys whose mail carries a credential: the prefixes, the names, and a
-- word between dots and dashes (or two such words joined: sign-in, log-in).
CREATE FUNCTION template_key_content_locked(k TEXT) RETURNS BOOLEAN
    LANGUAGE sql
    IMMUTABLE
    SET search_path = pg_catalog, public
AS
$$
WITH list AS (SELECT ARRAY ['login', 'signin', 'code', 'otp', 'passcode', 'password', 'reset', 'verify',
                            'verification', 'confirm', 'confirmation', 'token', 'magic', 'invite', 'activation',
                            'activate', '2fa', 'mfa', 'ticket', 'link',
                            'giris', 'kod', 'kodu', 'dogrulama', 'sifre', 'parola', 'baglanti', 'baglantisi', 'davet',
                            'onay'] AS words)
SELECT k IS NOT NULL
           AND (k ~ '^(keycloak|account|auth|login|otp)\.'
        OR k = ANY (ARRAY ['core.contact-consent-confirm', 'event.ticket-created', 'event.reminder'])
        OR EXISTS (SELECT 1
                   FROM (SELECT w, lead(w) OVER (ORDER BY n) AS next_w
                         FROM regexp_split_to_table(k, '[.-]') WITH ORDINALITY AS s(w, n)
                         WHERE w <> '') parts,
                        list
                   WHERE parts.w = ANY (list.words)
                      OR parts.w || COALESCE(parts.next_w, '') = ANY (list.words)))
$$;

-- Whether a send's content is shown masked: its template's key is locked, or
-- its template said so when it was sent, or says so now; a template that
-- said nothing leaves it to the send (a single mail is, a send to many is
-- not). A task that does not exist is sensitive.
CREATE OR REPLACE FUNCTION mail_task_content_sensitive(for_task UUID) RETURNS BOOLEAN
    LANGUAGE sql
    STABLE
    SET search_path = pg_catalog, public
AS
$$
SELECT COALESCE((SELECT template_key_content_locked(t.key)
                            OR COALESCE(t.content_sensitive, NOT mail_task_bulk(mt.id))
                            OR CASE mt.content_sensitive_at_send
                                   WHEN 'true' THEN true
                                   WHEN 'false' THEN false
                                   ELSE NOT mail_task_bulk(mt.id) END
                 FROM mail_tasks mt
                          LEFT JOIN templates t ON t.id = mt.template_id
                 WHERE mt.id = for_task),
                true)
$$;

-- A template made sensitive (or given a locked key) after a send: the send is
-- looked at again, and masked if it is sensitive now.
CREATE FUNCTION mail_tasks_mask_due_on_template_change() RETURNS TRIGGER
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public
AS
$$
BEGIN
    UPDATE mail_tasks
    SET content_mask_due = true
    WHERE template_id = NEW.id
      AND NOT content_mask_due
      AND content_masked_at IS NULL
      AND content_cleared_at IS NULL;
    RETURN NULL;
END
$$;

CREATE TRIGGER templates_mask_due
    AFTER UPDATE OF content_sensitive, key
    ON templates
    FOR EACH ROW
    WHEN (OLD.content_sensitive IS DISTINCT FROM NEW.content_sensitive OR OLD.key IS DISTINCT FROM NEW.key)
EXECUTE FUNCTION mail_tasks_mask_due_on_template_change();

CREATE TABLE sent_content_views
(
    id               UUID PRIMARY KEY     DEFAULT gen_random_uuid(),
    task_id          UUID        NOT NULL REFERENCES mail_tasks (id),
    queue_item_id    UUID        NOT NULL,
    -- The Keycloak subject of the token that opened it, and the name it
    -- carried then.
    actor_sub        TEXT,
    actor_name       TEXT,
    masked_by        TEXT        NOT NULL CHECK (masked_by IN ('variables', 'patterns')),
    masked_variables TEXT[]      NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sent_content_views_task ON sent_content_views (task_id, created_at DESC);
CREATE INDEX idx_sent_content_views_actor ON sent_content_views (actor_sub);

COMMENT ON TABLE sent_content_views IS
    'Each opening of a sensitive send''s masked content: who, which send and queue row, when. Account erasure replaces the actor.';

RESET lock_timeout;
