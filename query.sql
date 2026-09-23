-- name: CreateTemplate :one
INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content, key)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(key)::text)
RETURNING *;

-- name: GetTemplateByKey :one
SELECT *
FROM templates
WHERE key = $1
  AND archived_at IS NULL;

-- name: UpsertTemplateByKey :one
INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, system,
                       contract_required_variables)
VALUES (sqlc.arg(key)::text,
        sqlc.arg(name)::text,
        sqlc.arg(subject)::text,
        sqlc.arg(html_content)::text,
        sqlc.arg(plain_text_content)::text,
        sqlc.arg(react_email_content)::text,
        sqlc.arg(system)::boolean,
        COALESCE(sqlc.narg(contract_required_variables)::jsonb, '[]'))
-- The seed writes everything it sends, the subject included. It is the
-- caller that keeps an operator's change — the subject too — from being
-- overwritten: it refuses the seed before it gets here unless it is forced
-- (ADR-0047), which is what replaced leaving the subject alone on every seed.
ON CONFLICT (key) DO UPDATE
    SET name                = EXCLUDED.name,
        subject             = EXCLUDED.subject,
        html_content        = EXCLUDED.html_content,
        plain_text_content  = EXCLUDED.plain_text_content,
        react_email_content = EXCLUDED.react_email_content,
        system              = EXCLUDED.system,
        -- The contract set is the seed's, like the key: a seed that sends one
        -- replaces it (sorted, each name once, by the caller), a seed that
        -- sends none (NULL) leaves it. A name the contract now declares leaves
        -- the operators' set, so the two never share one and it shows as
        -- locked.
        contract_required_variables = COALESCE(sqlc.narg(contract_required_variables)::jsonb,
                                               templates.contract_required_variables),
        operator_required_variables = ARRAY(SELECT name
                                            FROM unnest(templates.operator_required_variables) AS name
                                            WHERE name <> ALL (contract_variable_names(
                                                    COALESCE(sqlc.narg(contract_required_variables)::jsonb,
                                                             templates.contract_required_variables)))
                                            ORDER BY name COLLATE "C"),
        archived_at         = NULL,
        archived_by         = NULL,
        -- A seed that goes through settles any seed refused before it.
        seed_refused_at             = NULL,
        seed_refused_rules          = NULL,
        seed_refused_payload_sha256 = NULL,
        updated_at          = NOW()
RETURNING *;

-- Keeps on a template that a Template seed was refused, and why. Refusing the
-- content refused last time again keeps when it was first refused; other
-- content starts over. Nothing that is sent changes, so updated_at stays.
-- name: RecordSeedRefusal :exec
UPDATE templates
SET seed_refused_at             = CASE
                                      WHEN seed_refused_payload_sha256 = sqlc.arg(payload_sha256)::text
                                          THEN seed_refused_at
                                      ELSE NOW()
    END,
    seed_refused_rules          = sqlc.arg(rules)::text[],
    seed_refused_payload_sha256 = sqlc.arg(payload_sha256)::text
WHERE id = sqlc.arg(id);

-- name: GetTemplateById :one
SELECT *
FROM templates
WHERE id = $1
  AND archived_at IS NULL;

-- name: GetTemplateByIdIncludingArchived :one
SELECT *
FROM templates
WHERE id = $1;

-- name: GetAllTemplates :many
SELECT *
FROM templates
WHERE archived_at IS NULL
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountTemplates :one
SELECT count(*)
FROM templates
WHERE archived_at IS NULL;

-- name: GetArchivedTemplates :many
SELECT *
FROM templates
WHERE archived_at IS NOT NULL
ORDER BY archived_at DESC
LIMIT $1 OFFSET $2;

-- name: CountArchivedTemplates :one
SELECT count(*)
FROM templates
WHERE archived_at IS NOT NULL;

-- name: GetAllTemplatesIncludingArchived :many
SELECT *
FROM templates
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountAllTemplatesIncludingArchived :one
SELECT count(*)
FROM templates;

-- name: UpdateTemplate :one
UPDATE templates
SET name                = $2,
    subject             = $3,
    html_content        = $4,
    plain_text_content  = $5,
    react_email_content = $6,
    key                 = sqlc.narg(key)::text,
    updated_at          = NOW()
WHERE id = $1
  AND archived_at IS NULL
RETURNING *;

-- name: ArchiveTemplate :one
UPDATE templates
SET archived_by = CASE
                      WHEN archived_at IS NULL THEN sqlc.narg(archived_by)::text
                      ELSE archived_by
                  END,
    archived_at = COALESCE(archived_at, NOW()),
    updated_at = CASE WHEN archived_at IS NULL THEN NOW() ELSE updated_at END
WHERE id = sqlc.arg(id)
  AND system = false
RETURNING *;

-- Required variable sets are sorted byte by byte (COLLATE "C"), the order the
-- handler sorts a contract set in and a missing list comes in, whatever the
-- database's collation.
--
-- Marks a variable of a template in use as required by operators. A name
-- already in either set changes nothing: one the contract declares is required
-- already, and stays the contract's. Takes the row's lock, so the caller can
-- check the body against the sets it returns before committing.
-- name: AddOperatorRequiredVariable :one
UPDATE templates
SET operator_required_variables = CASE
                                      WHEN sqlc.arg(name)::text = ANY (contract_variable_names(contract_required_variables))
                                          THEN operator_required_variables
                                      ELSE ARRAY(SELECT DISTINCT n COLLATE "C"
                                                 FROM unnest(array_append(operator_required_variables, sqlc.arg(name)::text)) AS n
                                                 ORDER BY n COLLATE "C")
    END
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
RETURNING *;

-- Releases a variable operators marked. It cannot release a contract one: the
-- sets never share a name, so a contract name is simply not in the operators'
-- set, and the caller sees it in the contract set it returns.
-- name: RemoveOperatorRequiredVariable :one
UPDATE templates
SET operator_required_variables = array_remove(operator_required_variables, sqlc.arg(name)::text)
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
RETURNING *;

-- name: RestoreTemplate :one
UPDATE templates
SET archived_at = NULL,
    archived_by = NULL,
    updated_at = CASE WHEN archived_at IS NULL THEN updated_at ELSE NOW() END
WHERE id = $1
RETURNING *;


-- Records what a template row now holds as a new Mail template version,
-- published at once, and makes the row a copy of it. This is the expand step
-- for the writers that still write the row directly — the old panel's create
-- and edit, and the Template seed's by-key upsert: each runs this after its
-- row write, in the same transaction, so the version is what the row ended up
-- with, not what the request asked for.
--
-- Those writers send a subject, a render and at most a JSX source, so the
-- version starts from the published one and replaces only what they changed:
--   * The subject and the render are the row's.
--   * If the body — html_content, plain_text_content and the JSX source — is
--     the published version's, the Main source and every source stay as they
--     were: the old panel sends a stored body back untouched when only the
--     wording around it changed.
--   * Otherwise the body is new. react_email_content with a JSX source in it
--     (template_jsx_source decides) makes JSX the Main source with that text;
--     without one — the seed's pointer comment, or nothing — html_content is
--     the HTML source and the Main source.
--   * Sources in the other Authoring modes are carried over.
-- A row with no published version yet — written before versions were kept,
-- or by the old binary between the migration and this one — is taken as it
-- now is. When the result is the published version over again, nothing is
-- recorded: the write changed nothing a version holds (name is not one).
--
-- The version is numbered after the template's last one; the row write before
-- this holds the row's lock, so two writers cannot take the same number. Its
-- base is the version the row was a copy of until now. A Template seed's
-- version also keeps the subject the seed sent, requested_subject.
--
-- Affects one row when a version was recorded and none when not.
-- name: RecordTemplateRowAsVersion :execrows
WITH written AS (SELECT t.id,
                        t.subject,
                        t.html_content,
                        t.plain_text_content,
                        t.published_version_id,
                        template_jsx_source(t.react_email_content) AS jsx_source
                 FROM templates t
                 WHERE t.id = sqlc.arg(template_id)),
     candidate AS (SELECT w.id                                                        AS template_id,
                          w.subject,
                          COALESCE(w.jsx_source, p.jsx_source)                        AS jsx_source,
                          p.visual_source,
                          CASE
                              WHEN body.kept OR w.jsx_source IS NOT NULL THEN p.html_source
                              ELSE w.html_content
                              END                                                     AS html_source,
                          CASE
                              WHEN body.kept THEN p.main_mode
                              WHEN w.jsx_source IS NOT NULL THEN 'jsx'::authoring_mode
                              ELSE 'html'::authoring_mode
                              END                                                     AS main_mode,
                          w.html_content,
                          w.plain_text_content,
                          p.id                                                        AS published_id,
                          (p.subject, p.jsx_source, p.visual_source, p.html_source, p.main_mode,
                           p.html_content, p.plain_text_content)                      AS published_content
                   FROM written w
                            LEFT JOIN template_versions p ON p.id = w.published_version_id
                            CROSS JOIN LATERAL (SELECT p.id IS NOT NULL
                                                           AND w.html_content = p.html_content
                                                           AND w.plain_text_content = p.plain_text_content
                                                           AND w.jsx_source IS NOT DISTINCT FROM p.jsx_source AS kept) body),
     version AS (
         INSERT INTO template_versions (template_id, seq, subject, jsx_source, visual_source, html_source, main_mode,
                                        html_content, plain_text_content, author_kind, author_sub, author_name,
                                        requested_subject, published_at, base_version_id)
             SELECT c.template_id,
                    COALESCE((SELECT max(v.seq) FROM template_versions v WHERE v.template_id = c.template_id), 0) + 1,
                    c.subject,
                    c.jsx_source,
                    c.visual_source,
                    c.html_source,
                    c.main_mode,
                    c.html_content,
                    c.plain_text_content,
                    sqlc.arg(author_kind)::template_author_kind,
                    sqlc.narg(author_sub)::text,
                    sqlc.narg(author_name)::text,
                    sqlc.narg(requested_subject)::text,
                    NOW(),
                    c.published_id
             FROM candidate c
             WHERE c.published_id IS NULL
                OR (c.subject, c.jsx_source, c.visual_source, c.html_source, c.main_mode,
                    c.html_content, c.plain_text_content) IS DISTINCT FROM c.published_content
             RETURNING id, template_id)
UPDATE templates t
SET published_version_id = version.id
FROM version
WHERE t.id = version.template_id;

-- A template's Mail template versions, newest first, without their sources or
-- render. A NULL published lists every version; true only published ones,
-- false only drafts, discarded ones included.
-- name: ListTemplateVersions :many
SELECT *
FROM template_version_summaries
WHERE template_id = $1
  AND (sqlc.narg(published)::boolean IS NULL OR (published_at IS NOT NULL) = sqlc.narg(published)::boolean)
ORDER BY seq DESC
LIMIT $2 OFFSET $3;

-- name: CountTemplateVersions :one
SELECT count(*)
FROM template_versions
WHERE template_id = $1
  AND (sqlc.narg(published)::boolean IS NULL OR (published_at IS NOT NULL) = sqlc.narg(published)::boolean);

-- One version of one template, whole. A version of another template is not
-- found here.
-- name: GetTemplateVersion :one
SELECT sqlc.embed(s),
       v.jsx_source,
       v.visual_source,
       v.html_source,
       v.html_content,
       v.plain_text_content
FROM template_version_summaries s
         JOIN template_versions v ON v.id = s.id
WHERE s.template_id = sqlc.arg(template_id)
  AND s.id = sqlc.arg(id);

-- Takes a template row's lock for a write that does not change the row first —
-- saving a draft, restoring a version, publishing, discarding. A version is
-- numbered after the lock is taken, and the old panel's and the seed's writes
-- take the same lock by updating the row, so every writer of one template
-- numbers its version in turn. An archived template is not found here, as it
-- is not for any other write.
-- name: LockTemplate :one
SELECT *
FROM templates
WHERE id = $1
  AND archived_at IS NULL
    FOR UPDATE;

-- Takes the lock of the template a Template key names, archived or not, so
-- the seed's upsert can judge the template before it writes: every other
-- writer of the template waits until it has. No row: the key is new.
-- name: LockTemplateByKey :one
SELECT *
FROM templates
WHERE key = $1
    FOR UPDATE;

-- The last version a Template seed wrote of a template. Seed versions are
-- always published, so this is the seed's last word on the template.
-- name: LastTemplateSeedVersion :one
SELECT *
FROM template_version_summaries
WHERE template_id = $1
  AND author_kind = 'template_seed'
ORDER BY seq DESC
LIMIT 1;

-- An operator's versions of a template numbered after the given one, oldest
-- first, drafts included and discarded drafts left out: operator work a seed
-- written after them would pass over.
-- name: ListOperatorVersionsAfter :many
SELECT *
FROM template_version_summaries
WHERE template_id = sqlc.arg(template_id)
  AND author_kind = 'operator'
  AND discarded_at IS NULL
  AND seq > sqlc.arg(after_seq)::int
ORDER BY seq;

-- name: GetTemplateVersionSummary :one
SELECT *
FROM template_version_summaries
WHERE template_id = $1
  AND id = $2;

-- Each operator's draft in progress on the given templates, newest first: the
-- newest version an operator wrote of a template, when it is neither published
-- nor discarded. An operator's later version supersedes their earlier drafts,
-- so those are not listed, and discarding their newest leaves them none; a
-- draft that someone else's publish made stale still is listed, until its
-- author writes again or discards it.
-- name: ListTemplateDrafts :many
SELECT *
FROM template_version_summaries s
WHERE s.id IN (SELECT DISTINCT ON (v.template_id, v.author_sub) v.id
               FROM template_versions v
               WHERE v.template_id = ANY (sqlc.arg(template_ids)::uuid[])
                 AND v.author_kind = 'operator'
               ORDER BY v.template_id, v.author_sub, v.seq DESC)
  AND s.published_at IS NULL
  AND s.discarded_at IS NULL
ORDER BY s.template_id, s.seq DESC;

-- The Authoring mode of each template's Main source as it is sent: its
-- published version's.
-- name: ListPublishedMainModes :many
SELECT t.id AS template_id, v.main_mode
FROM templates t
         JOIN template_versions v ON v.id = t.published_version_id
WHERE t.id = ANY (sqlc.arg(template_ids)::uuid[]);

-- Whether text is a JSX source by the rule the migration and the old panel's
-- writes read react_email_content with: something other than whitespace and
-- comments is left in it.
-- name: IsJSXSource :one
SELECT (template_jsx_source(sqlc.arg(content)::text) IS NOT NULL)::boolean AS is_source;

-- Writes an operator's draft, numbered after the template's last version,
-- unless the version it continues holds exactly this content already —
-- subject, every source, Main source and render, a Visual document compared
-- as JSON rather than as text. Returns the draft it wrote, or the version it
-- would have repeated, and whether it wrote one. The caller holds the template
-- row's lock (LockTemplate).
-- name: RecordTemplateDraft :one
WITH repeated AS (SELECT v.id
                  FROM template_versions v
                  WHERE v.id = sqlc.narg(continued_id)::uuid
                    AND (v.subject, v.jsx_source, v.visual_source, v.html_source, v.main_mode, v.html_content,
                         v.plain_text_content)
                      IS NOT DISTINCT FROM
                        (sqlc.arg(subject)::text, sqlc.narg(jsx_source)::text, sqlc.narg(visual_source)::jsonb,
                         sqlc.narg(html_source)::text, sqlc.arg(main_mode)::authoring_mode, sqlc.arg(html_content)::text,
                         sqlc.arg(plain_text_content)::text)),
     written AS (
         INSERT INTO template_versions (template_id, seq, subject, jsx_source, visual_source, html_source, main_mode,
                                        html_content, plain_text_content, author_kind, author_sub, author_name,
                                        base_version_id)
             SELECT sqlc.arg(template_id)::uuid,
                    COALESCE((SELECT max(v.seq) FROM template_versions v WHERE v.template_id = sqlc.arg(template_id)::uuid),
                             0) + 1,
                    sqlc.arg(subject)::text,
                    sqlc.narg(jsx_source)::text,
                    sqlc.narg(visual_source)::jsonb,
                    sqlc.narg(html_source)::text,
                    sqlc.arg(main_mode)::authoring_mode,
                    sqlc.arg(html_content)::text,
                    sqlc.arg(plain_text_content)::text,
                    'operator',
                    sqlc.narg(author_sub)::text,
                    sqlc.narg(author_name)::text,
                    sqlc.narg(base_version_id)::uuid
             WHERE NOT EXISTS (SELECT 1 FROM repeated)
             RETURNING id)
SELECT id, true AS written
FROM written
UNION ALL
SELECT id, false AS written
FROM repeated;

-- Publishes a draft: marks it published and copies it onto the template row,
-- which the send path reads — its subject and its render. react_email_content,
-- the column the old panel edits, gets the JSX source only when JSX is the
-- Main source, and an empty string otherwise. The old panel re-renders any JSX
-- it finds there and saves that render as the body, and the expand step would
-- then make JSX the Main source: a JSX source kept beside another Main source
-- would reach live mail without anyone choosing it. With nothing there, the
-- old panel refuses to save (it never saves an empty JSX source), so a
-- template whose Main source is not JSX is edited in the editor only. The
-- caller holds the row's lock and has checked that the version is a draft of
-- this template.
-- name: PublishTemplateDraft :one
WITH published AS (
    UPDATE template_versions v
        SET published_at = NOW()
        WHERE v.id = sqlc.arg(version_id)
            AND v.template_id = sqlc.arg(template_id)
            AND v.published_at IS NULL
            AND v.discarded_at IS NULL
        RETURNING v.id, v.template_id, v.subject, v.jsx_source, v.main_mode, v.html_content, v.plain_text_content)
UPDATE templates t
SET subject              = p.subject,
    html_content         = p.html_content,
    plain_text_content   = p.plain_text_content,
    react_email_content  = CASE WHEN p.main_mode = 'jsx' THEN p.jsx_source ELSE '' END,
    published_version_id = p.id,
    updated_at           = NOW()
FROM published p
WHERE t.id = p.template_id
RETURNING t.*;

-- Discards a draft: it stays in the history, but it is nobody's draft in
-- progress any more and it is never published. A draft discarded already
-- keeps the time it was. The caller holds the template row's lock and has
-- checked that the version is a draft of this template.
-- name: DiscardTemplateDraft :exec
UPDATE template_versions
SET discarded_at = COALESCE(discarded_at, NOW())
WHERE template_id = sqlc.arg(template_id)
  AND id = sqlc.arg(id)
  AND published_at IS NULL;

-- name: CreateMailingList :one
INSERT INTO mailing_lists (name)
VALUES ($1)
RETURNING *;

-- name: GetMailingListById :one
SELECT *
FROM mailing_lists
WHERE id = $1
  AND archived_at IS NULL;

-- name: GetMailingListByIdIncludingArchived :one
SELECT *
FROM mailing_lists
WHERE id = $1;

-- name: GetAllMailingLists :many
SELECT *
FROM mailing_lists
WHERE archived_at IS NULL
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountMailingLists :one
SELECT count(*)
FROM mailing_lists
WHERE archived_at IS NULL;

-- name: GetArchivedMailingLists :many
SELECT *
FROM mailing_lists
WHERE archived_at IS NOT NULL
ORDER BY archived_at DESC
LIMIT $1 OFFSET $2;

-- name: CountArchivedMailingLists :one
SELECT count(*)
FROM mailing_lists
WHERE archived_at IS NOT NULL;

-- name: GetAllMailingListsIncludingArchived :many
SELECT *
FROM mailing_lists
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountAllMailingListsIncludingArchived :one
SELECT count(*)
FROM mailing_lists;

-- name: UpdateMailingList :one
UPDATE mailing_lists
SET name       = $2,
    updated_at = NOW()
WHERE id = $1
  AND archived_at IS NULL
RETURNING *;

-- name: ArchiveMailingList :one
UPDATE mailing_lists
SET archived_by = CASE
                      WHEN archived_at IS NULL THEN sqlc.narg(archived_by)::text
                      ELSE archived_by
                  END,
    archived_at = COALESCE(archived_at, NOW()),
    updated_at = CASE WHEN archived_at IS NULL THEN NOW() ELSE updated_at END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: RestoreMailingList :one
UPDATE mailing_lists
SET archived_at = NULL,
    archived_by = NULL,
    updated_at = CASE WHEN archived_at IS NULL THEN updated_at ELSE NOW() END
WHERE id = $1
RETURNING *;


-- name: AddRecipientToMailingList :one
WITH target_list AS (
    SELECT mailing_lists.id
    FROM mailing_lists
    WHERE mailing_lists.id = sqlc.arg(mail_list_id)
      AND archived_at IS NULL
), recipient AS (
    INSERT INTO recipients (full_name, email)
        SELECT sqlc.arg(full_name), sqlc.arg(email) FROM target_list
        ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name
        RETURNING id, full_name, email, created_at, updated_at),
     association AS (
         INSERT INTO mailing_list_recipients (mail_list_id, recipient_id)
             SELECT sqlc.arg(mail_list_id), id FROM recipient
             ON CONFLICT DO NOTHING)
SELECT *
FROM recipient;

-- name: GetRecipientsByMailingListId :many
SELECT r.*
FROM recipients r
         JOIN mailing_list_recipients mlr ON r.id = mlr.recipient_id
         JOIN mailing_lists ml ON ml.id = mlr.mail_list_id
WHERE mlr.mail_list_id = $1
  AND ml.archived_at IS NULL
ORDER BY r.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountRecipientsByMailingListId :one
SELECT count(*)
FROM recipients r
         JOIN mailing_list_recipients mlr ON r.id = mlr.recipient_id
         JOIN mailing_lists ml ON ml.id = mlr.mail_list_id
WHERE mlr.mail_list_id = $1
  AND ml.archived_at IS NULL;

-- name: GetRecipients :many
SELECT *
FROM recipients
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountRecipients :one
SELECT count(*)
FROM recipients;

-- name: GetRecipientByEmail :one
SELECT *
FROM recipients
WHERE email = $1;

-- name: UpdateRecipient :one
UPDATE recipients
SET full_name  = $2,
    email      = $3,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: RemoveRecipientFromMailingListByID :exec
DELETE
FROM mailing_list_recipients
WHERE mail_list_id = $1
  AND recipient_id = $2;

-- name: ProcessQueueItems :many
UPDATE mail_queue
SET status = 'processing'
WHERE id IN (SELECT id
             FROM mail_queue
             WHERE status = 'pending'
               AND next_attempt_at <= NOW()
             ORDER BY next_attempt_at, created_at
             LIMIT 100 FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: ResetDeadJobs :exec
UPDATE mail_queue
SET status = 'pending'
WHERE status = 'processing';

-- name: SetMailQueueItemSent :exec
UPDATE mail_queue
SET status    = 'sent',
    error     = NULL
WHERE id = $1;

-- name: SetMailQueueItemFailed :exec
UPDATE mail_queue
SET status    = 'failed',
    attempts  = attempts + 1,
    error     = $2
WHERE id = $1;

-- name: RescheduleMailQueueItem :one
UPDATE mail_queue
SET status          = 'pending',
    attempts        = attempts + 1,
    error           = sqlc.narg(error)::text,
    next_attempt_at = NOW() + (sqlc.arg(delay_seconds)::int * INTERVAL '1 second')
WHERE id = sqlc.arg(id)
RETURNING attempts;

-- name: CreateMailTask :many
WITH active_source AS (
    SELECT t.id AS template_id, ml.id AS mail_list_id
    FROM templates t
             JOIN mailing_lists ml ON ml.id = sqlc.narg(mail_list_id)::uuid
    WHERE t.id = sqlc.narg(template_id)::uuid
      AND t.archived_at IS NULL
      AND ml.archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables)
        SELECT sqlc.arg(sent_by), template_id, mail_list_id, sqlc.arg(body_variables) FROM active_source
        RETURNING *
)
SELECT it.id               AS task_id,
       it.body_variables,
       t.name              AS template_name,
       t.subject           AS template_subject,
       t.html_content,
       t.plain_text_content,
       r.full_name         AS recipient_full_name,
       r.email             AS recipient_email
FROM inserted_task it
         JOIN templates t ON it.template_id = t.id
         JOIN mailing_list_recipients mlr ON it.mail_list_id = mlr.mail_list_id
         JOIN recipients r ON mlr.recipient_id = r.id;

-- name: CreateMailQueueItems :copyfrom
INSERT INTO mail_queue (
    task_id, recipient_full_name, recipient_email,
    subject, body, body_html
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetMailTaskById :one
SELECT mt.*,
       t.name  AS template_name,
       ml.name AS mail_list_name
FROM mail_tasks mt
         LEFT JOIN templates t ON mt.template_id = t.id
         LEFT JOIN mailing_lists ml ON mt.mail_list_id = ml.id
WHERE mt.id = $1;

-- name: CountMailTasks :one
SELECT count(*)
FROM mail_tasks;

-- A send's recipients, newest first. A list send's rows come from one insert
-- and share a created_at, so the id breaks the tie: pages neither repeat nor
-- skip a recipient. A NULL status lists every recipient.
-- name: GetMailQueueItemsByTaskId :many
SELECT id, recipient_full_name, recipient_email, status, error, attempts, next_attempt_at, created_at
FROM mail_queue
WHERE task_id = $1
  AND (sqlc.narg(status)::mail_queue_status IS NULL OR status = sqlc.narg(status)::mail_queue_status)
ORDER BY created_at DESC, id DESC
LIMIT $2 OFFSET $3;

-- name: CountMailQueueItemsByTaskId :one
SELECT count(*)
FROM mail_queue
WHERE task_id = $1
  AND (sqlc.narg(status)::mail_queue_status IS NULL OR status = sqlc.narg(status)::mail_queue_status);


-- name: InsertMailTask :one
INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables)
SELECT sqlc.arg(sent_by), sqlc.arg(template_id), sqlc.narg(mail_list_id), sqlc.arg(body_variables)
FROM templates
WHERE templates.id = sqlc.arg(template_id)
  AND archived_at IS NULL
RETURNING mail_tasks.id, sent_by, template_id, mail_list_id, body_variables, created_at;

-- name: CreateSingleMailTask :one
WITH active_template AS (
    SELECT templates.id
    FROM templates
    WHERE templates.id = sqlc.narg(template_id)::uuid
      AND archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, body_variables)
        SELECT sqlc.arg(sent_by), id, sqlc.arg(body_variables) FROM active_template
        RETURNING *
)
SELECT it.id               AS task_id,
       it.body_variables,
       t.name              AS template_name,
       t.subject           AS template_subject,
       t.html_content,
       t.plain_text_content,
       sqlc.arg(recipient_full_name)::text AS recipient_full_name,
       sqlc.arg(recipient_email)::text AS recipient_email
FROM inserted_task it
         JOIN templates t ON it.template_id = t.id;

-- name: CountMailQueueByStatus :one
SELECT (SELECT count(*) FROM mail_queue WHERE status = 'pending')    AS pending,
       (SELECT count(*) FROM mail_queue WHERE status = 'processing') AS processing,
       (SELECT count(*) FROM mail_queue WHERE status = 'sent')       AS sent,
       (SELECT count(*) FROM mail_queue WHERE status = 'failed')     AS failed;

-- A queue row has no sent time of its own. created_at — when that recipient's
-- mail was queued — stands in for it: the dispatcher is woken on enqueue, so a
-- mail that goes through on its first attempt leaves within seconds, and only a
-- retried one (the ladder tops out under eight minutes) can land later.
-- next_attempt_at is not used: every row older than the retry migration holds
-- that migration's timestamp in it.
-- Days are calendar days in time_zone; the series ends on as_of's day and has
-- one row per day, zero-filled.
-- name: GetDailySentCounts :many
WITH today AS (SELECT (sqlc.arg(as_of)::timestamptz AT TIME ZONE sqlc.arg(time_zone)::text)::date AS day),
     days AS (SELECT today.day - back AS day
              FROM today,
                   generate_series(0, sqlc.arg(days)::int - 1) AS back)
SELECT d.day::date        AS day,
       count(q.created_at) AS sent
FROM days d
         LEFT JOIN mail_queue q
                   ON q.status = 'sent'
                       AND q.created_at >= (d.day::timestamp AT TIME ZONE sqlc.arg(time_zone)::text)
                       AND q.created_at < ((d.day + 1)::timestamp AT TIME ZONE sqlc.arg(time_zone)::text)
GROUP BY d.day
ORDER BY d.day;

-- A send as every screen shows it — the home screen, the send list and a
-- send's own page: the task, the template it used, who it went to, its status
-- as mail_task_status derives it, and its recipients by status. A NULL task_id
-- lists every send and a NULL status every status. The page is cut first so
-- only its rows are counted.
-- name: ListMailTaskSends :many
WITH page AS (SELECT mt.id
              FROM mail_tasks mt
              WHERE (sqlc.narg(task_id)::uuid IS NULL OR mt.id = sqlc.narg(task_id)::uuid)
                AND (sqlc.narg(status)::text IS NULL OR mail_task_status(mt.id) = sqlc.narg(status)::text)
              ORDER BY mt.created_at DESC, mt.id DESC
              LIMIT $1 OFFSET $2)
SELECT mt.id,
       mt.sent_by,
       mt.template_id,
       mt.mail_list_id,
       mt.body_variables,
       mt.created_at,
       t.name                       AS template_name,
       t.key                        AS template_key,
       ml.name                      AS mail_list_name,
       (ml.id IS NOT NULL)::boolean AS internal_mail_list,
       mail_task_status(mt.id)      AS status,
       rc.pending,
       rc.processing,
       rc.sent,
       rc.failed,
       single.recipient_full_name   AS single_recipient_full_name,
       single.recipient_email       AS single_recipient_email
FROM page
         JOIN mail_tasks mt ON mt.id = page.id
         LEFT JOIN templates t ON mt.template_id = t.id
         LEFT JOIN mailing_lists ml ON mt.mail_list_id = ml.id
         CROSS JOIN LATERAL (SELECT count(*) FILTER (WHERE q.status = 'pending')    AS pending,
                                    count(*) FILTER (WHERE q.status = 'processing') AS processing,
                                    count(*) FILTER (WHERE q.status = 'sent')       AS sent,
                                    count(*) FILTER (WHERE q.status = 'failed')     AS failed
                             FROM mail_queue q
                             WHERE q.task_id = mt.id) rc
         LEFT JOIN mail_queue single
                   ON single.id = (SELECT q.id
                                   FROM mail_queue q
                                   WHERE mt.mail_list_id IS NULL
                                     AND q.task_id = mt.id
                                   ORDER BY q.created_at, q.id
                                   LIMIT 1)
ORDER BY mt.created_at DESC, mt.id DESC;

-- Sends by the status mail_task_status derives, over every send: the same
-- numbers CountMailTaskSends gives for each status filter.
-- name: CountMailTasksByStatus :one
SELECT count(*) FILTER (WHERE s.status = 'failed')  AS failed,
       count(*) FILTER (WHERE s.status = 'sending') AS sending,
       count(*) FILTER (WHERE s.status = 'sent')    AS sent
FROM (SELECT mail_task_status(mt.id) AS status FROM mail_tasks mt) s;

-- name: CountMailTaskSends :one
SELECT count(*)
FROM mail_tasks mt
WHERE (sqlc.narg(status)::text IS NULL OR mail_task_status(mt.id) = sqlc.narg(status)::text);

