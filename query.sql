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
                       contract_required_variables, content_sensitive, sensitive_variables)
VALUES (sqlc.arg(key)::text,
        sqlc.arg(name)::text,
        sqlc.arg(subject)::text,
        sqlc.arg(html_content)::text,
        sqlc.arg(plain_text_content)::text,
        sqlc.arg(react_email_content)::text,
        sqlc.arg(system)::boolean,
        COALESCE(sqlc.narg(contract_required_variables)::jsonb, '[]'),
        sqlc.narg(content_sensitive)::boolean,
        sqlc.narg(sensitive_variables)::text[])
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
        -- Whether a send's content is shown is the seed's too: a seed that
        -- says (true or false) sets it, a seed that does not (NULL) leaves it.
        content_sensitive   = COALESCE(sqlc.narg(content_sensitive)::boolean, templates.content_sensitive),
        -- So are the variables a send's masking hides whole: a seed that
        -- sends a list (sorted, each name once, by the caller; [] clears it)
        -- replaces it, a seed that sends none (NULL) leaves it.
        sensitive_variables = COALESCE(sqlc.narg(sensitive_variables)::text[], templates.sensitive_variables),
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
-- Sets whether the content of the template's sends is shown, as the panel
-- does; like a Required variable, a setting of the row: no version, and
-- updated_at stays. An archived template is not found.
-- name: SetTemplateContentSensitive :one
UPDATE templates
SET content_sensitive = sqlc.arg(content_sensitive)::boolean
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
RETURNING *;

-- name: RecordTemplateContentSensitiveEvent :exec
INSERT INTO template_content_sensitive_events (template_id, actor_kind, actor_sub, actor_name, old_value, new_value)
VALUES (sqlc.arg(template_id), sqlc.arg(actor_kind), sqlc.narg(actor_sub), sqlc.narg(actor_name),
        sqlc.narg(old_value), sqlc.narg(new_value));

-- A template's content_sensitive changes, newest first.
-- name: ListTemplateContentSensitiveEvents :many
SELECT id, template_id, actor_kind, actor_sub, actor_name, old_value, new_value, created_at
FROM template_content_sensitive_events
WHERE template_id = sqlc.arg(template_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountTemplateContentSensitiveEvents :one
SELECT count(*)
FROM template_content_sensitive_events
WHERE template_id = sqlc.arg(template_id);

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
--   * The name, the subject and the render are the row's.
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
-- recorded: the write changed nothing a version holds.
--
-- The version is numbered after the template's last one; the row write before
-- this holds the row's lock, so two writers cannot take the same number. Its
-- base is the version the row was a copy of until now. A Template seed's
-- version also keeps the subject the seed sent, requested_subject.
--
-- Affects one row when a version was recorded and none when not.
-- name: RecordTemplateRowAsVersion :execrows
WITH written AS (SELECT t.id,
                        t.name,
                        t.subject,
                        t.html_content,
                        t.plain_text_content,
                        t.published_version_id,
                        template_jsx_source(t.react_email_content) AS jsx_source
                 FROM templates t
                 WHERE t.id = sqlc.arg(template_id)),
     candidate AS (SELECT w.id                                                        AS template_id,
                          w.name,
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
                          (p.name, p.subject, p.jsx_source, p.visual_source, p.html_source, p.main_mode,
                           p.html_content, p.plain_text_content)                      AS published_content
                   FROM written w
                            LEFT JOIN template_versions p ON p.id = w.published_version_id
                            CROSS JOIN LATERAL (SELECT p.id IS NOT NULL
                                                           AND w.html_content = p.html_content
                                                           AND w.plain_text_content = p.plain_text_content
                                                           AND w.jsx_source IS NOT DISTINCT FROM p.jsx_source AS kept) body),
     version AS (
         INSERT INTO template_versions (template_id, seq, name, subject, jsx_source, visual_source, html_source, main_mode,
                                        html_content, plain_text_content, author_kind, author_sub, author_name,
                                        requested_subject, published_at, base_version_id)
             SELECT c.template_id,
                    COALESCE((SELECT max(v.seq) FROM template_versions v WHERE v.template_id = c.template_id), 0) + 1,
                    c.name,
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
                OR (c.name, c.subject, c.jsx_source, c.visual_source, c.html_source, c.main_mode,
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
-- name, subject, every source, Main source and render, a Visual document compared
-- as JSON rather than as text. Returns the draft it wrote, or the version it
-- would have repeated, and whether it wrote one. The caller holds the template
-- row's lock (LockTemplate).
-- name: RecordTemplateDraft :one
WITH repeated AS (SELECT v.id
                  FROM template_versions v
                  WHERE v.id = sqlc.narg(continued_id)::uuid
                    AND (v.name, v.subject, v.jsx_source, v.visual_source, v.html_source, v.main_mode, v.html_content,
                         v.plain_text_content)
                      IS NOT DISTINCT FROM
                        (sqlc.arg(name)::text, sqlc.arg(subject)::text, sqlc.narg(jsx_source)::text, sqlc.narg(visual_source)::jsonb,
                         sqlc.narg(html_source)::text, sqlc.arg(main_mode)::authoring_mode, sqlc.arg(html_content)::text,
                         sqlc.arg(plain_text_content)::text)),
     written AS (
         INSERT INTO template_versions (template_id, seq, name, subject, jsx_source, visual_source, html_source, main_mode,
                                        html_content, plain_text_content, author_kind, author_sub, author_name,
                                        base_version_id)
             SELECT sqlc.arg(template_id)::uuid,
                    COALESCE((SELECT max(v.seq) FROM template_versions v WHERE v.template_id = sqlc.arg(template_id)::uuid),
                             0) + 1,
                    sqlc.arg(name)::text,
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
-- which the send path reads — its name, subject and render. react_email_content,
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
        RETURNING v.id, v.template_id, v.name, v.subject, v.jsx_source, v.main_mode, v.html_content, v.plain_text_content)
UPDATE templates t
SET name                 = p.name,
    subject              = p.subject,
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

-- Takes up to max_rows due rows of the system lane (priority above 0: single
-- mail a person is waiting for) for one process, leased to it: claimed_by
-- names the process, claimed_at starts the lease. The process asks for as
-- many rows as it has idle workers, the system lane first, so no row waits
-- taken in a process's memory and no system row waits behind a bulk one. The
-- lane is written out, not a parameter, so the planner reads the lane's own
-- index (idx_mail_queue_pending_system) whatever plan it keeps.
-- name: ProcessSystemQueueItems :many
UPDATE mail_queue
SET status     = 'processing',
    claimed_at = NOW(),
    claimed_by = sqlc.arg(claimed_by)::text
WHERE id IN (SELECT id
             FROM mail_queue
             WHERE status = 'pending'
               AND priority > 0
               AND next_attempt_at <= NOW()
             ORDER BY next_attempt_at, created_at
             LIMIT sqlc.arg(max_rows)::int FOR UPDATE SKIP LOCKED)
RETURNING *;

-- The same for the bulk lane (lists, groups, approved sends), which reads
-- idx_mail_queue_pending_bulk. The process asks for no more rows than the
-- send budget leaves room for (SendWindowCounts), under LockSendBudget.
-- name: ProcessBulkQueueItems :many
UPDATE mail_queue
SET status     = 'processing',
    claimed_at = NOW(),
    claimed_by = sqlc.arg(claimed_by)::text
WHERE id IN (SELECT id
             FROM mail_queue
             WHERE status = 'pending'
               AND priority <= 0
               AND next_attempt_at <= NOW()
             ORDER BY next_attempt_at, created_at
             LIMIT sqlc.arg(max_rows)::int FOR UPDATE SKIP LOCKED)
RETURNING *;

-- The database's clock: the one clock every process's send budget reads, so
-- replicas whose clocks differ still count in the same windows. The
-- statement's time, not the transaction's (NOW()), so a budget read later in
-- a transaction reads the instant it counts at.
-- name: DatabaseNow :one
SELECT statement_timestamp()::timestamptz AS now;

-- Serialises the bulk lane's take across processes: under it a process reads
-- the send windows, takes as many bulk rows as they leave room for and counts
-- them, and the next process reads its count. Held to the end of the
-- transaction. Never waited for: false when another process holds it, and
-- the bulk lane skips that turn, so a process stuck holding it never holds up
-- another's dispatcher (nor its single mail). The system lane never takes it.
-- name: LockSendBudget :one
SELECT pg_try_advisory_xact_lock(hashtextextended('skymail-backend send budget', 0)) AS locked;

-- The send windows a budget reads at one instant: the minutes starting at or
-- after minute_since (the current minute and the one before it) and the
-- hours starting at or after hour_since (every hour of the last 24).
-- name: SendWindowCounts :many
SELECT span_seconds, starts_at, lane, taken
FROM mail_send_windows
WHERE (span_seconds = 60 AND starts_at >= sqlc.arg(minute_since)::timestamptz)
   OR (span_seconds = 3600 AND starts_at >= sqlc.arg(hour_since)::timestamptz);

-- Counts taken rows of one lane in the minute starting at minute_start and
-- the hour starting at hour_start.
-- name: CountSends :exec
INSERT INTO mail_send_windows (span_seconds, starts_at, lane, taken)
VALUES (60, sqlc.arg(minute_start)::timestamptz, sqlc.arg(lane)::smallint, sqlc.arg(taken)::int),
       (3600, sqlc.arg(hour_start)::timestamptz, sqlc.arg(lane)::smallint, sqlc.arg(taken)::int)
ON CONFLICT (span_seconds, starts_at, lane) DO UPDATE
    SET taken = mail_send_windows.taken + EXCLUDED.taken;

-- Drops send windows no budget reads any more: those that started before
-- before (two days back, well past the last 24 hours).
-- name: PruneSendWindows :execrows
DELETE
FROM mail_send_windows
WHERE starts_at < sqlc.arg(before)::timestamptz;

-- A processing row without claimed_at was taken by an image from before the
-- lease, or comes from a dump made before it. Its lease starts now, the first
-- time a process sees it: the image that took it may be sending it still.
-- name: StartLeaseOnUnclaimedJobs :execrows
UPDATE mail_queue
SET claimed_at = NOW()
WHERE status = 'processing'
  AND claimed_at IS NULL;

-- Puts back to pending the processing rows whose lease has run out: the
-- process that took them is gone, or a restored dump holds them so. A row
-- another process took within the lease is left to it.
-- name: ResetDeadJobs :execrows
UPDATE mail_queue
SET status     = 'pending',
    claimed_at = NULL,
    claimed_by = NULL
WHERE status = 'processing'
  AND claimed_at < NOW() - (sqlc.arg(lease_seconds)::int * INTERVAL '1 second');

-- Gives back a row this process took and never began to send: at shutdown,
-- a row the dispatcher handed to the workers' channel that no worker took.
-- It is pending again at once, with its attempts and due time as they were,
-- so another process sends it without waiting out the lease. Fenced like the
-- outcomes: only this claim of the row.
-- name: ReleaseMailQueueItem :execrows
UPDATE mail_queue
SET status     = 'pending',
    claimed_at = NULL,
    claimed_by = NULL
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND claimed_by = sqlc.arg(claimed_by)::text
  AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- After a restore from backup (docs/data-lifecycle.md, Backup and restore),
-- the queue as the dump held it: the rows still to go out, pending or
-- processing, that were queued before the restore instant. A row without
-- created_at counts as one of them: SkyMail stamps every row it queues, so
-- only the dump can hold such a row. left_queued is the rest of the queue,
-- queued since, which is never touched. Counts and times only.
-- name: CountRestoredQueueItems :one
WITH queued AS (SELECT task_id,
                       status,
                       created_at,
                       (created_at IS NULL OR created_at < sqlc.arg(before)::timestamptz) AS restored
                FROM mail_queue
                WHERE status IN ('pending', 'processing'))
SELECT count(*) FILTER (WHERE restored AND status = 'pending')    AS pending,
       count(*) FILTER (WHERE restored AND status = 'processing') AS processing,
       count(DISTINCT task_id) FILTER (WHERE restored)            AS tasks,
       (SELECT created_at FROM queued WHERE restored AND created_at IS NOT NULL
        ORDER BY created_at LIMIT 1)                              AS oldest,
       (SELECT created_at FROM queued WHERE restored AND created_at IS NOT NULL
        ORDER BY created_at DESC LIMIT 1)                         AS newest,
       count(*) FILTER (WHERE restored AND created_at IS NULL)    AS undated,
       count(*) FILTER (WHERE NOT restored)                       AS left_queued
FROM queued;

-- Closes the rows CountRestoredQueueItems counts as restored without sending
-- them: failed, with the error the send's page shows. mail_task_status then
-- reports their sends as failed. attempts is left as it was: nothing tried
-- them. It counts what it closed by the status each row had.
-- name: CloseRestoredQueueItems :one
WITH closing AS (SELECT id, status AS was
                 FROM mail_queue
                 WHERE status IN ('pending', 'processing')
                   AND (created_at IS NULL OR created_at < sqlc.arg(before)::timestamptz)
                 FOR UPDATE),
     closed AS (UPDATE mail_queue q
                SET status = 'failed',
                    error  = sqlc.arg(error)::text
                FROM closing c
                WHERE q.id = c.id
                RETURNING c.was, q.task_id, q.created_at)
SELECT count(*) FILTER (WHERE was = 'pending')    AS pending,
       count(*) FILTER (WHERE was = 'processing') AS processing,
       count(DISTINCT task_id)                     AS tasks,
       (SELECT created_at FROM closed WHERE created_at IS NOT NULL
        ORDER BY created_at LIMIT 1)               AS oldest,
       (SELECT created_at FROM closed WHERE created_at IS NOT NULL
        ORDER BY created_at DESC LIMIT 1)          AS newest,
       count(*) FILTER (WHERE created_at IS NULL)  AS undated
FROM closed;

-- The three outcomes of a send are written only by the claim that holds the
-- row: still processing, claimed by this process, at the claimed_at the claim
-- returned. A claim whose lease ran out changes nothing once the row was put
-- back or taken again, by another process or by this one (claimed_at differs
-- then); zero rows says so. A sent or failed row keeps its claim: the process
-- that finished it.
-- name: SetMailQueueItemSent :execrows
UPDATE mail_queue
SET status    = 'sent',
    error     = NULL
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND claimed_by = sqlc.arg(claimed_by)::text
  AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: SetMailQueueItemFailed :execrows
UPDATE mail_queue
SET status    = 'failed',
    attempts  = attempts + 1,
    error     = sqlc.narg(error)::text
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND claimed_by = sqlc.arg(claimed_by)::text
  AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: RescheduleMailQueueItem :one
UPDATE mail_queue
SET status          = 'pending',
    attempts        = attempts + 1,
    error           = sqlc.narg(error)::text,
    next_attempt_at = NOW() + (sqlc.arg(delay_seconds)::int * INTERVAL '1 second'),
    claimed_at      = NULL,
    claimed_by      = NULL
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND claimed_by = sqlc.arg(claimed_by)::text
  AND claimed_at = sqlc.arg(claimed_at)::timestamptz
RETURNING attempts;

-- name: CreateMailTask :many
WITH active_source AS (
    SELECT t.id AS template_id, ml.id AS mail_list_id, COALESCE(t.content_sensitive::text, 'unset') AS content_sensitive
    FROM templates t
             JOIN mailing_lists ml ON ml.id = sqlc.narg(mail_list_id)::uuid
    WHERE t.id = sqlc.narg(template_id)::uuid
      AND t.archived_at IS NULL
      AND ml.archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, content_sensitive_at_send)
        SELECT sqlc.arg(sent_by), template_id, mail_list_id, sqlc.arg(body_variables), content_sensitive FROM active_source
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

-- A send's rows. Every row queued here is an e-mail: the channel is left to
-- its default ('email'); a channel that queues its own rows names it.
-- recipient_sub is the recipient's Keycloak subject when the send knew it.
-- name: CreateMailQueueItems :copyfrom
INSERT INTO mail_queue (
    task_id, recipient_full_name, recipient_email,
    subject, body, body_html, priority, recipient_sub
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- A recipient whose mail did not render, say a template that slices an empty
-- full name, is written failed, with the render error the send's page shows:
-- the send's other recipients are queued and mail_task_status reports the
-- send failed. Nothing was tried, so attempts stays 0.
-- name: CreateFailedMailQueueItem :exec
INSERT INTO mail_queue (
    task_id, recipient_full_name, recipient_email,
    subject, body, priority, status, error, recipient_sub
) VALUES (
    sqlc.arg(task_id), sqlc.arg(recipient_full_name), sqlc.arg(recipient_email),
    '', '', sqlc.arg(priority), 'failed', sqlc.arg(error)::text, sqlc.narg(recipient_sub)::uuid
);

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
SELECT id, channel, recipient_full_name, recipient_email, status, error, attempts, next_attempt_at, created_at
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


-- One recipient's mail as it is kept — as sent, or masked by the content
-- sweep — with what decides who may see it: the send's sender, whether it is
-- bulk, whether its content is sensitive, whether the sweep has masked or
-- emptied it.
-- name: GetMailQueueItemContent :one
SELECT q.id,
       q.task_id,
       q.status,
       q.subject,
       q.body,
       q.body_html,
       q.created_at,
       mt.sent_by,
       mt.created_at                               AS task_created_at,
       mt.content_cleared_at,
       mt.content_masked_at,
       mt.content_masked_by,
       mt.content_masked_variables,
       mail_task_bulk(mt.id)::boolean              AS bulk,
       mail_task_content_sensitive(mt.id)::boolean AS content_sensitive,
       t.key                                       AS template_key
FROM mail_queue q
         JOIN mail_tasks mt ON mt.id = q.task_id
         LEFT JOIN templates t ON t.id = mt.template_id
WHERE q.task_id = sqlc.arg(task_id)
  AND q.id = sqlc.arg(id);

-- The next send the content sweep has to decide on masking (content_mask_due,
-- idx_mail_tasks_content_mask_due): not masked or emptied, five minutes old,
-- none of whose rows is still to go out, not among those it already tried in
-- this run, and, if its masking failed before, due to be tried again. The
-- sends that never failed go first. sensitive says whether to mask it;
-- sensitive_variables are the variables its template lists as sensitive
-- (empty: none listed). SKIP LOCKED lets a second replica's sweep take
-- another.
-- name: TakeSendToMask :many
SELECT mt.id,
       mt.template_id,
       mt.body_variables,
       mt.created_at,
       mt.content_mask_failures,
       mail_task_content_sensitive(mt.id)::boolean      AS sensitive,
       COALESCE(t.sensitive_variables, '{}')::text[] AS sensitive_variables
FROM mail_tasks mt
         LEFT JOIN templates t ON t.id = mt.template_id
WHERE mt.content_mask_due
  AND mt.content_masked_at IS NULL
  AND mt.content_cleared_at IS NULL
  AND mt.created_at < now() - INTERVAL '5 minutes'
  AND NOT EXISTS (SELECT 1
                  FROM mail_queue q
                  WHERE q.task_id = mt.id
                    AND q.status IN ('pending', 'processing'))
  AND NOT (mt.id = ANY (sqlc.arg(tried)::uuid[]))
  AND (mt.content_mask_retry_at IS NULL OR mt.content_mask_retry_at <= now())
ORDER BY mt.content_mask_retry_at NULLS FIRST, mt.created_at
LIMIT 1 FOR UPDATE OF mt SKIP LOCKED;

-- A send found not sensitive: not looked at again until its template changes.
-- Its failures from when it was sensitive go: made sensitive again, it starts
-- afresh, its deadline too (#65 review, K2).
-- name: MarkSendNotDueForMasking :exec
UPDATE mail_tasks
SET content_mask_due           = false,
    content_mask_failures      = 0,
    content_mask_unconfirmed   = 0,
    content_mask_retry_at      = NULL,
    content_mask_failing_since = NULL
WHERE id = sqlc.arg(id);

-- Puts a send whose masking failed back: not counted yet, tried again after
-- base_seconds doubled for each failure it has had (a day at most). The first
-- failure since the send was last masked starts its deadline.
-- name: DeferMaskFailure :exec
UPDATE mail_tasks
SET content_mask_unconfirmed   = content_mask_unconfirmed + 1,
    content_mask_retry_at      = now() + LEAST(sqlc.arg(base_seconds)::float8
                                                   * power(2, content_mask_failures + content_mask_unconfirmed),
                                               86400) * INTERVAL '1 second',
    content_mask_failing_since = COALESCE(content_mask_failing_since, now())
WHERE id = sqlc.arg(id);

-- Counts the failure of each send that failed in a run that masked some other
-- send — the fault was the send's — and only that run's: a failure from a run
-- that masked none (the fault may have been SkyMail's) is never counted, so a
-- fault across SkyMail empties nothing once it is over. Those it leaves go
-- (content_mask_unconfirmed = 0): they would hold the send's retry at a day,
-- and an image from before this rule would count them all (#65 review, B2).
-- Returns each send and its count.
-- name: ConfirmMaskFailures :many
UPDATE mail_tasks
SET content_mask_failures    = content_mask_failures + 1,
    content_mask_unconfirmed = 0
WHERE id = ANY (sqlc.arg(ids)::uuid[])
  AND content_mask_unconfirmed > 0
  AND content_masked_at IS NULL
  AND content_cleared_at IS NULL
RETURNING id, content_mask_failures;

-- Empties up to batch_size sensitive sends whose masking has been failing for
-- longer than deadline_seconds, counted or not, the oldest first: their raw
-- mail is not kept (not_kept). Returns each send and since when it failed.
-- name: ClearSendsPastMaskDeadline :many
WITH due AS (SELECT mt.id, mt.content_mask_failing_since
             FROM mail_tasks mt
             WHERE mt.content_mask_failing_since < now() - sqlc.arg(deadline_seconds)::float8 * INTERVAL '1 second'
               AND mt.content_masked_at IS NULL
               AND mt.content_cleared_at IS NULL
               AND mail_task_content_sensitive(mt.id)
               AND NOT EXISTS (SELECT 1
                               FROM mail_queue q
                               WHERE q.task_id = mt.id
                                 AND q.status IN ('pending', 'processing'))
             ORDER BY mt.content_mask_failing_since
             LIMIT sqlc.arg(batch_size)::int FOR UPDATE OF mt SKIP LOCKED),
     emptied AS (UPDATE mail_queue q
         SET subject   = '',
             body      = '',
             body_html = NULL
         FROM due
         WHERE q.task_id = due.id
         RETURNING q.task_id)
UPDATE mail_tasks mt
SET body_variables     = '{}',
    content_mask_due   = false,
    content_cleared_at = now()
FROM due
WHERE mt.id = due.id
RETURNING mt.id, due.content_mask_failing_since::timestamptz AS failing_since;

-- A template's parts as they stood when a send was queued: the row, if it has
-- not been written since, and the newest version published by then. Never a
-- version written after: its render could depend on the hidden value.
-- name: ListTemplatePartsInForceAt :many
SELECT parts.subject, parts.plain_text_content, parts.html_content
FROM (SELECT t.subject, t.plain_text_content, t.html_content, 0 AS rank
      FROM templates t
      WHERE t.id = sqlc.arg(template_id)
        AND t.updated_at <= sqlc.arg(sent_at)
      UNION ALL
      (SELECT v.subject, v.plain_text_content, v.html_content, 1 AS rank
       FROM template_versions v
       WHERE v.template_id = sqlc.arg(template_id)
         AND v.published_at <= sqlc.arg(sent_at)
       ORDER BY v.published_at DESC, v.seq DESC
       LIMIT 1)) parts
ORDER BY parts.rank;

-- The next rows of a send that still hold a mail, after the row after, for the
-- sweep to mask; locked until it has, so Account erasure cannot empty one
-- between the reading and the writing (a row it is emptying is waited for and
-- read again: emptied, it is not taken).
-- name: ListQueueRowsToMask :many
SELECT id, recipient_full_name, recipient_email, subject, body, body_html
FROM mail_queue
WHERE task_id = sqlc.arg(task_id)
  AND id > sqlc.arg(after)
  AND (subject <> '' OR body <> '' OR body_html IS NOT NULL)
ORDER BY id
LIMIT sqlc.arg(row_limit) FOR UPDATE;

-- Puts each row's masked mail in place of what was sent; an empty HTML is
-- none. Only a row of the send that still holds a mail, of a send not masked
-- or emptied yet: never one Account erasure emptied.
-- name: SetMaskedQueueContent :exec
UPDATE mail_queue q
SET subject   = u.subject,
    body      = u.body,
    body_html = NULLIF(u.body_html, '')
FROM (SELECT unnest(sqlc.arg(ids)::uuid[])      AS id,
             unnest(sqlc.arg(subjects)::text[]) AS subject,
             unnest(sqlc.arg(bodies)::text[])   AS body,
             unnest(sqlc.arg(htmls)::text[])    AS body_html) u
WHERE q.id = u.id
  AND q.task_id = sqlc.arg(task_id)
  AND (q.subject <> '' OR q.body <> '' OR q.body_html IS NOT NULL)
  AND EXISTS (SELECT 1
              FROM mail_tasks mt
              WHERE mt.id = q.task_id
                AND mt.content_masked_at IS NULL
                AND mt.content_cleared_at IS NULL);

-- Marks a send masked and puts its masked variables in place of its own.
-- name: MarkSendMasked :exec
UPDATE mail_tasks
SET body_variables             = sqlc.arg(body_variables),
    content_mask_due           = false,
    content_mask_failures      = 0,
    content_mask_unconfirmed   = 0,
    content_mask_retry_at      = NULL,
    content_mask_failing_since = NULL,
    content_masked_at          = now(),
    content_masked_by          = sqlc.arg(masked_by),
    content_masked_variables   = sqlc.arg(masked_variables)::text[]
WHERE id = sqlc.arg(id);

-- Empties a send the sweep could not mask for certain, or failed to mask three
-- times: it is not kept.
-- name: ClearSendContent :exec
WITH emptied AS (UPDATE mail_queue
    SET subject = '', body = '', body_html = NULL
    WHERE mail_queue.task_id = sqlc.arg(task_id)
    RETURNING mail_queue.id)
UPDATE mail_tasks
SET body_variables     = '{}',
    content_mask_due   = false,
    content_cleared_at = now()
WHERE mail_tasks.id = sqlc.arg(task_id);

-- Deletes up to batch_size of the oldest records of who opened a masked mail
-- that are older than retention_days (SENT_CONTENT_VIEWS_RETENTION_DAYS).
-- name: DeleteOldSentContentViews :execrows
DELETE
FROM sent_content_views
WHERE id IN (SELECT v.id
             FROM sent_content_views v
             WHERE v.created_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
             ORDER BY v.created_at
             LIMIT sqlc.arg(batch_size)::int FOR UPDATE SKIP LOCKED);

-- Records that someone opened one recipient's masked mail of a sensitive send.
-- name: RecordSentContentView :exec
INSERT INTO sent_content_views (task_id, queue_item_id, actor_sub, actor_name, masked_by, masked_variables)
VALUES (sqlc.arg(task_id), sqlc.arg(queue_item_id), sqlc.narg(actor_sub), sqlc.narg(actor_name),
        sqlc.arg(masked_by), sqlc.arg(masked_variables)::text[]);

-- Empties the content of up to batch_size sends none of whose rows is still to
-- go out (pending or processing) once they are older than retention_days,
-- sensitive or not: a sensitive send's content is kept as long as any other's
-- and shown masked (2026-10-13). Their rows' subject and body become '' and
-- body_html NULL, as Account erasure leaves a row; the send's body_variables
-- become {}; content_cleared_at says when. Status, recipient, error and counts
-- stay. SKIP LOCKED lets a second replica's sweep take other sends. Returns
-- the sends emptied.
-- name: ClearDueMailContent :many
WITH due AS (SELECT mt.id
             FROM mail_tasks mt
             WHERE mt.content_cleared_at IS NULL
               -- Five minutes for the rows to be written: an image from before
               -- the send's task and rows were written in one transaction
               -- (#58) writes the task first, and a send emptied before its
               -- rows exist would keep them.
               AND mt.created_at < now() - INTERVAL '5 minutes'
               AND mt.created_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
               AND NOT EXISTS (SELECT 1
                               FROM mail_queue q
                               WHERE q.task_id = mt.id
                                 AND q.status IN ('pending', 'processing'))
             ORDER BY mt.created_at
             LIMIT sqlc.arg(batch_size)::int FOR UPDATE OF mt SKIP LOCKED),
     rows AS (UPDATE mail_queue q
         SET subject   = '',
             body      = '',
             body_html = NULL
         FROM due
         WHERE q.task_id = due.id
           AND (q.subject <> '' OR q.body <> '' OR q.body_html IS NOT NULL)
         RETURNING q.task_id)
UPDATE mail_tasks mt
SET body_variables     = '{}',
    content_cleared_at = now()
FROM due
WHERE mt.id = due.id
RETURNING mt.id, (SELECT count(*) FROM rows WHERE rows.task_id = mt.id) AS rows_cleared;

-- name: InsertMailTask :one
INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, content_sensitive_at_send)
SELECT sqlc.arg(sent_by), sqlc.arg(template_id), sqlc.narg(mail_list_id), sqlc.arg(body_variables),
       COALESCE(templates.content_sensitive::text, 'unset')
FROM templates
WHERE templates.id = sqlc.arg(template_id)
  AND archived_at IS NULL
RETURNING mail_tasks.id, sent_by, template_id, mail_list_id, body_variables, created_at;

-- name: CreateSingleMailTask :one
WITH active_template AS (
    SELECT templates.id, COALESCE(templates.content_sensitive::text, 'unset') AS content_sensitive
    FROM templates
    WHERE templates.id = sqlc.narg(template_id)::uuid
      AND archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send)
        SELECT sqlc.arg(sent_by), id, sqlc.arg(body_variables), content_sensitive FROM active_template
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
       single.recipient_email       AS single_recipient_email,
       mail_task_bulk(mt.id)::boolean              AS bulk,
       mail_task_content_sensitive(mt.id)::boolean AS content_sensitive,
       mt.content_cleared_at,
       mt.content_masked_at
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


-- A new Mail onayı request, pending until its deadline. Every later write of
-- a request runs in a transaction that first takes its row lock
-- (LockMailApproval), so its checks, its state change and its events happen
-- in turn, and an approval queues its send once.
-- A request to people is written with them (AddMailApprovalRecipients) in the
-- same transaction: it goes to a list or to people, which is checked when the
-- transaction commits.
-- name: CreateMailApproval :one
INSERT INTO mail_approvals (submitter_sub, submitter_name, submitter_email, submitter_email_unverified, template_id,
                            template_version_id, mail_list_id, body_variables, created_at, submitted_at, deadline_at,
                            updated_at)
VALUES (sqlc.arg(submitter_sub), sqlc.narg(submitter_name), sqlc.narg(submitter_email),
        sqlc.arg(submitter_email_unverified), sqlc.arg(template_id),
        sqlc.arg(template_version_id), sqlc.narg(mail_list_id), sqlc.arg(body_variables), sqlc.arg(at), sqlc.arg(at),
        sqlc.arg(deadline_at), sqlc.arg(at))
RETURNING *;

-- The people a request goes to, in the order submitted, replacing whoever it
-- went to before; none for a request to a list. emails and full_names pair up
-- by index.
-- name: ClearMailApprovalRecipients :exec
DELETE
FROM mail_approval_recipients
WHERE approval_id = $1;

-- name: AddMailApprovalRecipients :exec
INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
SELECT sqlc.arg(approval_id), p.position, p.email, (sqlc.arg(full_names)::text[])[p.position]
FROM unnest(sqlc.arg(emails)::text[]) WITH ORDINALITY AS p(email, position);

-- name: ListMailApprovalRecipients :many
SELECT email, full_name
FROM mail_approval_recipients
WHERE approval_id = $1
ORDER BY position;

-- The sends an approval queued, in order: a list's one send, or one per
-- person, each at that person's position.
-- name: AddMailApprovalTasks :exec
INSERT INTO mail_approval_tasks (approval_id, position, task_id)
SELECT sqlc.arg(approval_id), t.position, t.task_id
FROM unnest(sqlc.arg(task_ids)::uuid[]) WITH ORDINALITY AS t(task_id, position);

-- Takes a request's row lock without waiting for it: a request someone else is
-- deciding right now is refused (55P03) rather than queued behind them — the
-- lock is held while the send is queued, and a wait would hold a connection
-- that send needs.
-- name: LockMailApproval :one
SELECT *
FROM mail_approvals
WHERE id = $1
    FOR UPDATE NOWAIT;

-- The next undecided request past its deadline that no one is deciding right
-- now, locked for the expiry sweep.
-- name: LockDueMailApproval :one
SELECT *
FROM mail_approvals
WHERE state IN ('pending', 'returned')
  AND deadline_at <= sqlc.arg(as_of)
ORDER BY deadline_at, id
LIMIT 1 FOR UPDATE SKIP LOCKED;

-- A NULL deadline leaves the request's deadline as it is.
-- name: SetMailApprovalState :one
UPDATE mail_approvals
SET state       = sqlc.arg(state),
    deadline_at = COALESCE(sqlc.narg(deadline_at), deadline_at),
    updated_at  = sqlc.arg(at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetMailApprovalVariables :one
UPDATE mail_approvals
SET body_variables = sqlc.arg(body_variables),
    updated_at     = sqlc.arg(at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- A resubmission: what would be sent, as the submitter now fills it in,
-- pinned to the version published now, pending again with a new deadline. Its
-- people are written beside it (ClearMailApprovalRecipients, then
-- AddMailApprovalRecipients).
-- name: ResubmitMailApproval :one
UPDATE mail_approvals
SET state               = 'pending',
    template_id         = sqlc.arg(template_id),
    template_version_id = sqlc.arg(template_version_id),
    mail_list_id        = sqlc.narg(mail_list_id),
    body_variables      = sqlc.arg(body_variables),
    submitted_at        = sqlc.arg(at),
    deadline_at         = sqlc.arg(deadline_at),
    updated_at          = sqlc.arg(at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- Numbered after the request's last event; the caller holds its row lock.
-- name: RecordMailApprovalEvent :one
INSERT INTO mail_approval_events (approval_id, seq, kind, actor_sub, actor_name, note, changes, task_id, created_at)
SELECT sqlc.arg(approval_id),
       COALESCE(max(e.seq), 0) + 1,
       sqlc.arg(kind),
       sqlc.narg(actor_sub),
       sqlc.narg(actor_name),
       sqlc.narg(note),
       sqlc.narg(changes),
       sqlc.narg(task_id),
       sqlc.arg(at)
FROM mail_approval_events e
WHERE e.approval_id = sqlc.arg(approval_id)
RETURNING *;

-- A request as every screen shows it: with its template's name and key, the
-- version the template publishes now, its list's name when the list is an
-- internal one (a Keycloak group's is Keycloak's to give), the people it goes
-- to and the sends it queued, each in order.
-- name: GetMailApproval :one
SELECT sqlc.embed(a),
       t.name                             AS template_name,
       t.key                              AS template_key,
       t.published_version_id             AS template_published_version_id,
       (t.archived_at IS NOT NULL)::boolean AS template_archived,
       ml.name                            AS mail_list_name,
       (ml.id IS NOT NULL)::boolean       AS internal_mail_list,
       people.emails                      AS recipient_emails,
       people.full_names                  AS recipient_full_names,
       sends.task_ids                     AS task_ids
FROM mail_approvals a
         JOIN templates t ON t.id = a.template_id
         LEFT JOIN mailing_lists ml ON ml.id = a.mail_list_id
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(r.email ORDER BY r.position), '{}')::text[]     AS emails,
                                    COALESCE(array_agg(r.full_name ORDER BY r.position), '{}')::text[] AS full_names
                             FROM mail_approval_recipients r
                             WHERE r.approval_id = a.id) people
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(mat.task_id ORDER BY mat.position), '{}')::uuid[] AS task_ids
                             FROM mail_approval_tasks mat
                             WHERE mat.approval_id = a.id) sends
WHERE a.id = $1;

-- Requests newest submission first, the id breaking ties. A NULL submitter
-- lists everyone's and a NULL state every state. A request is filtered by the
-- state it is in as of as_of: one undecided past its deadline is expired,
-- whether or not the sweep has written it yet.
-- name: ListMailApprovals :many
SELECT sqlc.embed(a),
       t.name                             AS template_name,
       t.key                              AS template_key,
       t.published_version_id             AS template_published_version_id,
       (t.archived_at IS NOT NULL)::boolean AS template_archived,
       ml.name                            AS mail_list_name,
       (ml.id IS NOT NULL)::boolean       AS internal_mail_list,
       people.emails                      AS recipient_emails,
       people.full_names                  AS recipient_full_names,
       sends.task_ids                     AS task_ids
FROM mail_approvals a
         JOIN templates t ON t.id = a.template_id
         LEFT JOIN mailing_lists ml ON ml.id = a.mail_list_id
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(r.email ORDER BY r.position), '{}')::text[]     AS emails,
                                    COALESCE(array_agg(r.full_name ORDER BY r.position), '{}')::text[] AS full_names
                             FROM mail_approval_recipients r
                             WHERE r.approval_id = a.id) people
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(mat.task_id ORDER BY mat.position), '{}')::uuid[] AS task_ids
                             FROM mail_approval_tasks mat
                             WHERE mat.approval_id = a.id) sends
WHERE (sqlc.narg(submitter_sub)::text IS NULL OR a.submitter_sub = sqlc.narg(submitter_sub)::text)
  AND (sqlc.narg(state)::mail_approval_state IS NULL OR sqlc.narg(state)::mail_approval_state = (CASE
        WHEN a.state IN ('pending', 'returned') AND a.deadline_at <= sqlc.arg(as_of) THEN 'expired'
        ELSE a.state END))
ORDER BY a.submitted_at DESC, a.id DESC
LIMIT $1 OFFSET $2;

-- name: CountMailApprovals :one
SELECT count(*)
FROM mail_approvals a
WHERE (sqlc.narg(submitter_sub)::text IS NULL OR a.submitter_sub = sqlc.narg(submitter_sub)::text)
  AND (sqlc.narg(state)::mail_approval_state IS NULL OR sqlc.narg(state)::mail_approval_state = (CASE
        WHEN a.state IN ('pending', 'returned') AND a.deadline_at <= sqlc.arg(as_of) THEN 'expired'
        ELSE a.state END));

-- name: ListMailApprovalEvents :many
SELECT *
FROM mail_approval_events
WHERE approval_id = $1
ORDER BY seq;

-- The last event of each of the requests, for the list.
-- name: ListLastMailApprovalEvents :many
SELECT DISTINCT ON (approval_id) *
FROM mail_approval_events
WHERE approval_id = ANY (sqlc.arg(approval_ids)::uuid[])
ORDER BY approval_id, seq DESC;

-- Keeps a template from being published over, archived or changed while a
-- send of it is checked and queued; other sends of it share the lock.
-- name: ShareLockTemplate :one
SELECT *
FROM templates
WHERE id = $1
    FOR SHARE;

-- Keeps an internal list from being archived while a send to it is checked
-- and queued. No row: the id is not an internal list's.
-- name: ShareLockMailingList :one
SELECT *
FROM mailing_lists
WHERE id = $1
    FOR SHARE;
