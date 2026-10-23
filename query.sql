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
    error     = NULL,
    sent_at   = NOW()
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

-- content_sensitive_at_send is whether the send is sensitive as it is
-- queued: 'true' when its template's key is locked sensitive
-- (template_key_content_locked) whatever the template says, else the
-- template's content_sensitive ('unset' for NULL). It never changes after:
-- mail_task_content_sensitive() reads it first (20261020120000).
-- name: CreateMailTask :many
WITH active_source AS (
    SELECT t.id AS template_id, ml.id AS mail_list_id,
           CASE WHEN template_key_content_locked(t.key) THEN 'true' ELSE COALESCE(t.content_sensitive::text, 'unset') END AS content_sensitive
    FROM templates t
             JOIN mailing_lists ml ON ml.id = sqlc.narg(mail_list_id)::uuid
    WHERE t.id = sqlc.narg(template_id)::uuid
      AND t.archived_at IS NULL
      AND ml.archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, content_sensitive_at_send,
                            tracking_requested, purpose, category, transactional_statement, commercial_content_absent)
        SELECT sqlc.arg(sent_by), template_id, mail_list_id, sqlc.arg(body_variables), content_sensitive,
               sqlc.arg(tracking_requested)::boolean, sqlc.narg(purpose)::text, sqlc.narg(category)::text,
               sqlc.narg(transactional_statement)::boolean, sqlc.narg(commercial_content_absent)::boolean
        FROM active_source
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
-- tracking_token is the recipient's token when the row is tracked, and
-- tracking_skip_reason why not when the send asked for tracking.
-- list_unsubscribe_url is the recipient's one-click unsubscribe link when
-- core gave one (List-Unsubscribe, bounce ticket 11).
-- name: CreateMailQueueItems :copyfrom
INSERT INTO mail_queue (
    task_id, recipient_full_name, recipient_email,
    subject, body, body_html, priority, recipient_sub,
    tracking_token, tracking_skip_reason, suppressed_but_sent,
    list_unsubscribe_url
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

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

-- Whether a send is never tracked, whatever was asked: its template is a
-- System template, or its content is sensitive by the one definition the
-- content panel and the masking sweep use (mail_task_content_sensitive: the
-- template says so now or said so when the send was queued, or its key is
-- locked sensitive). A send without a template is never tracked.
-- name: GetMailTaskTrackingExclusion :one
SELECT COALESCE(t.system, true)::boolean                                       AS system_template,
       (t.id IS NULL OR mail_task_content_sensitive(mt.id))::boolean AS content_sensitive
FROM mail_tasks mt
         LEFT JOIN templates t ON t.id = mt.template_id
WHERE mt.id = sqlc.arg(task_id);

-- Whether a send of a template would never be tracked, asked before the send
-- exists so core is not asked about its people for nothing: a System
-- template, or content sensitive as mail_task_content_sensitive() reads it
-- for a send to many (bulk) or to one. GetMailTaskTrackingExclusion, in the
-- send's transaction, decides.
-- name: GetTemplateTrackingExclusion :one
SELECT t.system::boolean                                                                            AS system_template,
       (template_key_content_locked(t.key) OR COALESCE(t.content_sensitive, NOT sqlc.arg(bulk)::boolean))::boolean AS content_sensitive
FROM templates t
WHERE t.id = sqlc.arg(template_id);

-- A send that asked for tracking but can never be tracked (single mail, a
-- System or sensitive template) says it is not tracked: no token, no row
-- reasons, tracking_requested false, as TrackingApplies told the caller.
-- name: ClearMailTaskTracking :exec
UPDATE mail_tasks
SET tracking_requested = false
WHERE id = sqlc.arg(task_id);

-- A tracked send's links that are the same for every recipient, each with
-- its own token (open and click tracking).
-- name: CreateMailTaskLinks :copyfrom
INSERT INTO mail_task_links (task_id, link_token, url, position)
VALUES ($1, $2, $3, $4);

-- The links a tracked row's mail gets its click addresses for when it goes
-- out: the send's links whose address is still kept.
-- name: ListMailTaskLinkTargets :many
SELECT link_token, url::text AS url
FROM mail_task_links
WHERE task_id = sqlc.arg(task_id)
  AND url IS NOT NULL
ORDER BY position;

-- A tracked row whose mail goes out while SKYMAIL_TRACKING is off: it goes
-- out clean and its token is closed, so nothing is recorded for it should the
-- flag be turned on again.
-- name: CloseQueueTrackingFlagOff :execrows
UPDATE mail_queue
SET tracking_disabled_at = NOW(),
    tracking_skip_reason = 'flag_off'
WHERE id = sqlc.arg(id)
  AND tracking_token IS NOT NULL
  AND tracking_disabled_at IS NULL;

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
-- suppressed_but_sent: account mail sent to a suppressed address, flagged.
-- undelivered: a report said the address does not exist (a hard bounce of
-- the row's own address) after the relay took the mail.
-- name: GetMailQueueItemsByTaskId :many
SELECT id, channel, recipient_full_name, recipient_email, status, error, attempts, next_attempt_at, created_at,
       suppressed_but_sent,
       EXISTS (SELECT 1
               FROM mail_bounces b
               WHERE b.queue_id = mail_queue.id
                 AND b.kind = 'hard'
                 AND b.recipient_matched)::boolean AS undelivered
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
         SET subject              = '',
             body                 = '',
             body_html            = NULL,
             list_unsubscribe_url = NULL
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
SELECT id, recipient_full_name, recipient_email, subject, body, body_html, list_unsubscribe_url
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
    SET subject = '', body = '', body_html = NULL, list_unsubscribe_url = NULL
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
         SET subject              = '',
             body                 = '',
             body_html            = NULL,
             list_unsubscribe_url = NULL
         FROM due
         WHERE q.task_id = due.id
           AND (q.subject <> '' OR q.body <> '' OR q.body_html IS NOT NULL OR q.list_unsubscribe_url IS NOT NULL)
         RETURNING q.task_id)
UPDATE mail_tasks mt
SET body_variables     = '{}',
    content_cleared_at = now()
FROM due
WHERE mt.id = due.id
RETURNING mt.id, (SELECT count(*) FROM rows WHERE rows.task_id = mt.id) AS rows_cleared;

-- content_sensitive_at_send is whether the send is sensitive as it is
-- queued: 'true' when its template's key is locked sensitive
-- (template_key_content_locked) whatever the template says, else the
-- template's content_sensitive ('unset' for NULL). It never changes after:
-- mail_task_content_sensitive() reads it first (20261020120000).
-- name: InsertMailTask :one
INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, form_source_id, body_variables, content_sensitive_at_send,
                        tracking_requested, purpose, category, transactional_statement, commercial_content_absent)
SELECT sqlc.arg(sent_by), sqlc.arg(template_id), sqlc.narg(mail_list_id), sqlc.narg(form_source_id), sqlc.arg(body_variables),
       CASE WHEN template_key_content_locked(templates.key) THEN 'true' ELSE COALESCE(templates.content_sensitive::text, 'unset') END,
       sqlc.arg(tracking_requested)::boolean, sqlc.narg(purpose)::text, sqlc.narg(category)::text,
       sqlc.narg(transactional_statement)::boolean, sqlc.narg(commercial_content_absent)::boolean
FROM templates
WHERE templates.id = sqlc.arg(template_id)
  AND archived_at IS NULL
RETURNING mail_tasks.id, sent_by, template_id, mail_list_id, body_variables, created_at;

-- content_sensitive_at_send is whether the send is sensitive as it is
-- queued: 'true' when its template's key is locked sensitive
-- (template_key_content_locked) whatever the template says, else the
-- template's content_sensitive ('unset' for NULL). It never changes after:
-- mail_task_content_sensitive() reads it first (20261020120000).
-- name: CreateSingleMailTask :one
WITH active_template AS (
    SELECT templates.id,
           CASE WHEN template_key_content_locked(templates.key) THEN 'true' ELSE COALESCE(templates.content_sensitive::text, 'unset') END AS content_sensitive
    FROM templates
    WHERE templates.id = sqlc.narg(template_id)::uuid
      AND archived_at IS NULL
), inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
                            purpose, category, transactional_statement, commercial_content_absent)
        SELECT sqlc.arg(sent_by), id, sqlc.arg(body_variables), content_sensitive, sqlc.arg(tracking_requested)::boolean,
               sqlc.narg(purpose)::text, sqlc.narg(category)::text,
               sqlc.narg(transactional_statement)::boolean, sqlc.narg(commercial_content_absent)::boolean
        FROM active_template
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
       mt.form_source_id, fs.form_id, fs.form_title, fs.prepared_at AS form_prepared_at, fs.recipient_count AS form_recipient_count,
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
       rc.suppressed,
       rc.suppressed_but_sent,
       rc.undelivered,
       single.recipient_full_name   AS single_recipient_full_name,
       single.recipient_email       AS single_recipient_email,
       mail_task_bulk(mt.id)::boolean              AS bulk,
       mail_task_content_sensitive(mt.id)::boolean AS content_sensitive,
       mt.content_cleared_at,
       mt.content_masked_at,
       mt.tracking_requested,
       mt.purpose,
       mt.category,
       mt.transactional_statement,
       mt.commercial_content_absent
FROM page
         JOIN mail_tasks mt ON mt.id = page.id
         LEFT JOIN templates t ON mt.template_id = t.id
         LEFT JOIN mailing_lists ml ON mt.mail_list_id = ml.id
         LEFT JOIN mail_form_sources fs ON fs.id = mt.form_source_id
         CROSS JOIN LATERAL (SELECT count(*) FILTER (WHERE q.status = 'pending')    AS pending,
                                    count(*) FILTER (WHERE q.status = 'processing') AS processing,
                                    count(*) FILTER (WHERE q.status = 'sent')       AS sent,
                                    count(*) FILTER (WHERE q.status = 'failed')     AS failed,
                                    count(*) FILTER (WHERE q.status = 'failed' AND q.error LIKE 'suppressed:%') AS suppressed,
                                    count(*) FILTER (WHERE q.suppressed_but_sent) AS suppressed_but_sent,
                                    count(*) FILTER (WHERE EXISTS (SELECT 1
                                                                   FROM mail_bounces b
                                                                   WHERE b.queue_id = q.id
                                                                     AND b.kind = 'hard'
                                                                     AND b.recipient_matched)) AS undelivered
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
                            template_version_id, mail_list_id, form_source_id, body_variables, created_at, submitted_at, deadline_at,
                            updated_at, tracking_requested, purpose, category, transactional_statement,
                            commercial_content_absent)
VALUES (sqlc.arg(submitter_sub), sqlc.narg(submitter_name), sqlc.narg(submitter_email),
        sqlc.arg(submitter_email_unverified), sqlc.arg(template_id),
        sqlc.arg(template_version_id), sqlc.narg(mail_list_id), sqlc.narg(form_source_id), sqlc.arg(body_variables), sqlc.arg(at), sqlc.arg(at),
        sqlc.arg(deadline_at), sqlc.arg(at), sqlc.arg(tracking_requested)::boolean, sqlc.narg(purpose)::text,
        sqlc.narg(category)::text, sqlc.narg(transactional_statement)::boolean, sqlc.narg(commercial_content_absent)::boolean)
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
    form_source_id      = sqlc.narg(form_source_id),
    body_variables      = sqlc.arg(body_variables),
    tracking_requested  = sqlc.arg(tracking_requested)::boolean,
    purpose                   = sqlc.narg(purpose)::text,
    category                  = sqlc.narg(category)::text,
    transactional_statement   = sqlc.narg(transactional_statement)::boolean,
    commercial_content_absent = sqlc.narg(commercial_content_absent)::boolean,
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
       fs.form_id, fs.form_title, fs.prepared_at AS form_prepared_at,
       fs.recipient_count AS form_recipient_count, fs.snapshot_hash AS form_snapshot_hash,
       people.emails                      AS recipient_emails,
       people.full_names                  AS recipient_full_names,
       sends.task_ids                     AS task_ids
FROM mail_approvals a
         JOIN templates t ON t.id = a.template_id
         LEFT JOIN mailing_lists ml ON ml.id = a.mail_list_id
         LEFT JOIN mail_form_sources fs ON fs.id = a.form_source_id
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(r.email ORDER BY r.position), '{}')::text[]     AS emails,
                                    COALESCE(array_agg(r.full_name ORDER BY r.position), '{}')::text[] AS full_names
                             FROM (SELECT r.email, r.full_name, r.position FROM mail_approval_recipients r WHERE r.approval_id = a.id
                                   UNION ALL
                                   SELECT r.email, r.full_name, r.position FROM mail_form_source_recipients r WHERE r.source_id = a.form_source_id) r) people
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
       fs.form_id, fs.form_title, fs.prepared_at AS form_prepared_at,
       fs.recipient_count AS form_recipient_count, fs.snapshot_hash AS form_snapshot_hash,
       people.emails                      AS recipient_emails,
       people.full_names                  AS recipient_full_names,
       sends.task_ids                     AS task_ids
FROM mail_approvals a
         JOIN templates t ON t.id = a.template_id
         LEFT JOIN mailing_lists ml ON ml.id = a.mail_list_id
         LEFT JOIN mail_form_sources fs ON fs.id = a.form_source_id
         CROSS JOIN LATERAL (SELECT COALESCE(array_agg(r.email ORDER BY r.position), '{}')::text[]     AS emails,
                                    COALESCE(array_agg(r.full_name ORDER BY r.position), '{}')::text[] AS full_names
                             FROM (SELECT r.email, r.full_name, r.position FROM mail_approval_recipients r WHERE r.approval_id = a.id
                                   UNION ALL
                                   SELECT r.email, r.full_name, r.position FROM mail_form_source_recipients r WHERE r.source_id = a.form_source_id) r) people
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

-- Public click resolution is independent of the recipient and the flag: old
-- mail keeps working after withdrawal. No request data becomes a target.
-- name: ResolveTrackingLink :one
SELECT id, task_id, url, url_cleared_reason, created_at
FROM mail_task_links WHERE link_token = $1;

-- All writers lock the recipient before observing its event counts. Separate
-- statements after the lock take a fresh READ COMMITTED snapshot, so two
-- replicas cannot both pass the per-recipient caps on a stale snapshot.
-- name: LockTrackingRecipient :one
SELECT q.id, q.task_id, q.sent_at, now()::timestamptz AS at
FROM mail_queue q JOIN mail_tasks t ON t.id = q.task_id
WHERE q.tracking_token = $1 AND q.tracking_disabled_at IS NULL
  AND q.recipient_sub IS NOT NULL AND q.status = 'sent'
  AND q.sent_at IS NOT NULL AND q.sent_at > now() - interval '365 days'
  AND q.sent_at <= now() AND t.tracking_closed_at IS NULL
FOR UPDATE OF q;

-- name: TrackingHasOpen :one
SELECT EXISTS(SELECT 1 FROM mail_tracking_events WHERE queue_id = $1 AND kind = 'open')::boolean;

-- Limits live in SQL as well as the process-wide recording budget. There
-- are at most 20 opens and 50 clicks per recipient across every replica.
-- name: InsertTrackingEvent :execrows
INSERT INTO mail_tracking_events (queue_id, task_id, kind, link_id, occurred_at, source_class, automated_likely, classifier_version)
SELECT q.id, q.task_id, sqlc.arg(kind)::text, sqlc.narg(link_id)::bigint, sqlc.arg(at),
       sqlc.arg(source_class), sqlc.arg(automated_likely), sqlc.arg(classifier_version)
FROM mail_queue q
WHERE q.id = sqlc.arg(queue_id) AND q.tracking_disabled_at IS NULL
  AND q.recipient_sub IS NOT NULL AND q.status = 'sent'
  AND q.sent_at > sqlc.arg(at)::timestamptz - interval '365 days'
  AND q.sent_at <= sqlc.arg(at)
  AND NOT EXISTS (SELECT 1 FROM mail_tasks t WHERE t.id = q.task_id AND t.tracking_closed_at IS NOT NULL)
  AND ((sqlc.arg(kind)::text = 'open' AND sqlc.narg(link_id)::bigint IS NULL
        AND (SELECT count(*) FROM mail_tracking_events e WHERE e.queue_id = q.id AND e.kind = 'open') < 20
        AND NOT EXISTS (SELECT 1 FROM mail_tracking_events e WHERE e.queue_id = q.id AND e.kind = 'open'
                        AND e.occurred_at > sqlc.arg(at)::timestamptz - interval '5 minutes'))
    OR (sqlc.arg(kind)::text = 'click'
        AND EXISTS (SELECT 1 FROM mail_task_links l WHERE l.id = sqlc.narg(link_id)::bigint AND l.task_id = q.task_id
                    AND l.url IS NOT NULL AND l.created_at > sqlc.arg(at)::timestamptz - interval '1 year')
        AND (SELECT count(*) FROM mail_tracking_events e WHERE e.queue_id = q.id AND e.kind = 'click') < 50
        AND NOT EXISTS (SELECT 1 FROM mail_tracking_events e WHERE e.queue_id = q.id AND e.kind = 'click'
                        AND e.link_id = sqlc.narg(link_id)::bigint
                        AND e.occurred_at > sqlc.arg(at)::timestamptz - interval '1 minute')));

-- The third distinct link in five seconds identifies the whole burst, not
-- just its last click. Only this classifier version's contemporaneous events
-- change; newer rules never reinterpret historical events.
-- name: MarkTrackingScannerBurst :exec
UPDATE mail_tracking_events e SET source_class = 'scanner_likely', automated_likely = true
WHERE e.queue_id = sqlc.arg(queue_id) AND e.kind = 'click'
  AND e.classifier_version = sqlc.arg(classifier_version)
  AND e.occurred_at BETWEEN sqlc.arg(at)::timestamptz - interval '5 seconds' AND sqlc.arg(at)
  AND (SELECT count(DISTINCT b.link_id) FROM mail_tracking_events b
       WHERE b.queue_id = e.queue_id AND b.kind = 'click'
         AND b.classifier_version = sqlc.arg(classifier_version)
         AND b.occurred_at BETWEEN sqlc.arg(at)::timestamptz - interval '5 seconds' AND sqlc.arg(at)) >= 3;

-- system_lane: a row of the send is in the system lane (priority > 0, as
-- ProcessSystemQueueItems reads it). A tracked mail there is always one the
-- system sends (a service's single mail, SkyMail's own approval notice): a
-- person's tracked send is in the bulk lane, and the system lane is never
-- tracked as a person asks. Its people are read with the role only.
-- name: TrackingTaskAccess :one
SELECT t.id, t.sent_by, t.tracking_requested, t.tracking_closed_at,
       COALESCE((SELECT max(q.sent_at) FROM mail_queue q WHERE q.task_id = t.id), t.created_at)::timestamptz AS sent_at,
       COALESCE((SELECT min(q.sent_at) FROM mail_queue q WHERE q.task_id = t.id), t.created_at)::timestamptz AS started_at,
       EXISTS (SELECT 1 FROM mail_queue q WHERE q.task_id = t.id AND q.priority > 0) AS system_lane
FROM mail_tasks t WHERE t.id = $1;

-- Aggregates remain inside PostgreSQL: a report never loads raw events or
-- people into the HTTP process. Link URLs are omitted: one-recipient sends
-- can have personal URLs and the summary only requires mails:read.
-- name: GetTrackingReport :one
WITH people AS (
 SELECT q.id, q.tracking_token IS NOT NULL AS tracked, COALESCE(q.tracking_skip_reason,'not_requested') AS reason
 FROM mail_queue q WHERE q.task_id = sqlc.arg(task_id)
), events AS (
 SELECT e.* FROM mail_tracking_events e JOIN mail_queue q ON q.id = e.queue_id
 WHERE e.task_id = sqlc.arg(task_id) AND q.sent_at > now() - interval '365 days'
), reasons AS (
 SELECT reason, count(*) AS n FROM people WHERE NOT tracked GROUP BY reason
), links AS (
 SELECT l.id, l.position, count(e.id)::int AS clicks, count(DISTINCT e.queue_id)::int AS unique_clickers
 FROM mail_task_links l LEFT JOIN events e ON e.link_id = l.id
 WHERE l.task_id = sqlc.arg(task_id) GROUP BY l.id ORDER BY l.position
), buckets AS (
 SELECT CASE WHEN occurred_at < sqlc.arg(sent_at)::timestamptz + interval '72 hours'
             THEN date_trunc('hour',occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
             ELSE date_trunc('day',occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' END AS at,
        CASE WHEN occurred_at < sqlc.arg(sent_at)::timestamptz + interval '72 hours' THEN 'hour' ELSE 'day' END AS interval,
        count(*) FILTER (WHERE kind='open' AND NOT automated_likely)::int AS likely_human_opens,
        count(*) FILTER (WHERE kind='open' AND automated_likely)::int AS automated_opens,
        count(*) FILTER (WHERE kind='click' AND NOT automated_likely)::int AS likely_human_clicks,
        count(*) FILTER (WHERE kind='click' AND automated_likely)::int AS automated_clicks
 FROM events GROUP BY 1,2 ORDER BY 1,2
)
SELECT jsonb_build_object(
 'tracked_recipients',(SELECT count(*) FROM people WHERE tracked),
 'untracked_by_reason',COALESCE((SELECT jsonb_object_agg(reason,n) FROM reasons),'{}'::jsonb),
 'unique_opens',(SELECT count(DISTINCT queue_id) FROM events WHERE kind='open'),
 'unique_likely_human_opens',(SELECT count(DISTINCT queue_id) FROM events WHERE NOT automated_likely),
 'automated_only_recipients',(SELECT count(*) FROM (SELECT queue_id FROM events GROUP BY queue_id HAVING bool_and(automated_likely)) a),
 'unique_clickers',(SELECT count(DISTINCT queue_id) FROM events WHERE kind='click'),
 'clicks_by_link',COALESCE((SELECT jsonb_agg(to_jsonb(links)) FROM links),'[]'::jsonb),
 'series',COALESCE((SELECT jsonb_agg(to_jsonb(buckets)) FROM buckets),'[]'::jsonb)
)::jsonb AS report;

-- name: GetRetainedTrackingReport :one
SELECT jsonb_build_object('tracked_recipients',tracked_recipients,'untracked_by_reason',untracked_by_reason,
 'unique_opens',unique_opens,'unique_likely_human_opens',unique_likely_human_opens,
 'unique_clickers',unique_clickers,'clicks_by_link',clicks_by_link,'rolled_up_at',rolled_up_at)::jsonb AS report
FROM mail_task_tracking_totals WHERE task_id = $1;

-- One aggregate per recipient, then filtering and pagination. Events older
-- than their recipient's one-year window (tracking.EventRetentionDays) never
-- reappear while retention is disabled or catching up. No tracking token or link target is selected.
-- name: ListTrackingRecipients :one
WITH clicked AS (
 SELECT e.queue_id,e.link_id,count(*)::int AS clicks,max(e.occurred_at) AS last_clicked_at
 FROM mail_tracking_events e JOIN mail_queue q ON q.id=e.queue_id
 WHERE e.task_id=sqlc.arg(task_id) AND e.kind='click' AND q.sent_at > now() - interval '365 days'
 GROUP BY e.queue_id,e.link_id
), people AS (
 SELECT q.id,q.recipient_sub,q.recipient_email,q.recipient_full_name,
        q.tracking_token IS NOT NULL AS tracked,q.tracking_skip_reason,
        min(e.occurred_at) FILTER (WHERE e.kind='open') AS first_open_at,
        min(e.occurred_at) FILTER (WHERE NOT e.automated_likely AND e.kind='open') AS human_open_at,
        min(e.occurred_at) FILTER (WHERE NOT e.automated_likely AND e.kind='click') AS human_click_at,
        count(e.id) FILTER (WHERE e.kind='open')::int AS open_count,
        max(e.occurred_at) FILTER (WHERE e.kind='click') AS last_clicked_at,
        count(e.id)::int AS event_count,
        COALESCE(bool_and(e.automated_likely),false)::boolean AS automated_only,
        COALESCE(jsonb_agg(DISTINCT e.source_class) FILTER (WHERE e.id IS NOT NULL),'[]'::jsonb) AS source_classes,
        COALESCE((SELECT jsonb_agg(jsonb_build_object('link_id',c.link_id,'clicks',c.clicks,'last_clicked_at',c.last_clicked_at) ORDER BY c.link_id)
                  FROM clicked c WHERE c.queue_id=q.id),'[]'::jsonb) AS clicked_links
 FROM mail_queue q LEFT JOIN mail_tracking_events e ON e.queue_id=q.id AND q.sent_at > now() - interval '365 days'
 WHERE q.task_id=sqlc.arg(task_id)
 GROUP BY q.id
), filtered AS (
 SELECT * FROM people WHERE sqlc.arg(filter)::text='' OR
  (sqlc.arg(filter)::text='opened' AND (first_open_at IS NOT NULL OR human_click_at IS NOT NULL)) OR
  (sqlc.arg(filter)::text='not_opened' AND first_open_at IS NULL AND human_click_at IS NULL) OR
  (sqlc.arg(filter)::text='clicked' AND last_clicked_at IS NOT NULL) OR
  (sqlc.arg(filter)::text='automated_only' AND automated_only AND event_count>0)
), page AS (
 SELECT id,recipient_sub,recipient_email,recipient_full_name,tracked,tracking_skip_reason,
 first_open_at,COALESCE(human_open_at,human_click_at) AS first_likely_human_open_at,
 CASE WHEN human_open_at IS NULL AND human_click_at IS NOT NULL THEN 'click_inferred'
      WHEN human_open_at IS NOT NULL THEN 'open' ELSE NULL END AS likely_human_open_source,
 open_count,last_clicked_at,automated_only,source_classes,clicked_links
 FROM filtered ORDER BY recipient_full_name,id LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset)
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM filtered),'items',COALESCE((SELECT jsonb_agg(to_jsonb(page)) FROM page),'[]'::jsonb))::jsonb AS recipients;


-- Forms snapshots are prepared by the trusted service, owned by a person,
-- and bound exactly once inside that person's approval or send transaction.
-- name: CreateMailFormSource :one
INSERT INTO mail_form_sources(id, owner_sub, form_id, form_title, prepared_at, expires_at,
 response_count, recipient_count, skipped_count, duplicate_count, snapshot_hash)
VALUES(sqlc.arg(id),sqlc.arg(owner_sub),sqlc.arg(form_id),sqlc.arg(form_title),sqlc.arg(prepared_at),sqlc.arg(expires_at),
 sqlc.arg(response_count),sqlc.arg(recipient_count),sqlc.arg(skipped_count),sqlc.arg(duplicate_count),sqlc.arg(snapshot_hash))
RETURNING *;

-- name: AddMailFormSourceRecipients :exec
INSERT INTO mail_form_source_recipients(source_id,position,email,full_name)
SELECT sqlc.arg(source_id),p.position,p.email,(sqlc.arg(full_names)::text[])[p.position]
FROM unnest(sqlc.arg(emails)::text[]) WITH ORDINALITY AS p(email,position);

-- name: GetOwnedMailFormSource :one
SELECT * FROM mail_form_sources WHERE id=$1 AND owner_sub=$2;

-- name: LockOwnedMailFormSource :one
SELECT * FROM mail_form_sources WHERE id=$1 AND owner_sub=$2 FOR UPDATE NOWAIT;

-- name: LockMailFormSource :one
SELECT * FROM mail_form_sources WHERE id=$1 FOR UPDATE NOWAIT;

-- name: ListMailFormSourceRecipients :many
SELECT * FROM mail_form_source_recipients WHERE source_id=$1 ORDER BY position;

-- name: ConsumeMailFormSource :one
UPDATE mail_form_sources
SET consumed_at=sqlc.arg(at), consumed_approval_id=sqlc.narg(approval_id), consumed_task_id=sqlc.narg(task_id), consume_request_hash=sqlc.arg(request_hash)
WHERE id=sqlc.arg(id) AND consumed_at IS NULL AND invalidated_at IS NULL
RETURNING *;

-- name: DeleteExpiredMailFormSources :execrows
DELETE FROM mail_form_sources WHERE id IN (
 SELECT fs.id FROM mail_form_sources fs WHERE fs.consumed_at IS NULL AND fs.expires_at <= sqlc.arg(at)
 ORDER BY fs.expires_at,fs.id LIMIT sqlc.arg(batch_size) FOR UPDATE SKIP LOCKED
);

-- A notification type as the Template seed states it
-- (PUT /v1/notification_types/{key}): written over, its archive left as it
-- is.
-- name: UpsertNotificationType :one
INSERT INTO notification_types (key, category, required, default_channels, lane)
VALUES (sqlc.arg(key)::text, sqlc.narg(category)::text, sqlc.arg(required)::boolean,
        sqlc.arg(default_channels)::text[], sqlc.arg(lane)::text)
ON CONFLICT (key) DO UPDATE
    SET category         = EXCLUDED.category,
        required         = EXCLUDED.required,
        default_channels = EXCLUDED.default_channels,
        lane             = EXCLUDED.lane,
        updated_at       = now()
RETURNING *;

-- name: GetNotificationType :one
SELECT *
FROM notification_types
WHERE key = $1;

-- name: ListNotificationTypes :many
SELECT *
FROM notification_types
ORDER BY key;

-- A notification accepted under its caller's Idempotency-Key; nothing when
-- the caller already used the key (a concurrent request with it waits here
-- for the first to commit or roll back).
-- name: InsertNotification :one
INSERT INTO notifications (type, sent_by, idempotency_key, request_sha256, source_kind, source_ref)
VALUES (sqlc.arg(type)::text, sqlc.arg(sent_by)::text, sqlc.arg(idempotency_key)::text,
        sqlc.arg(request_sha256)::text, sqlc.narg(source_kind)::text, sqlc.narg(source_ref)::text)
ON CONFLICT (sent_by, idempotency_key) DO NOTHING
RETURNING id;

-- name: GetNotificationByIdempotencyKey :one
SELECT id, type, request_sha256, result
FROM notifications
WHERE sent_by = sqlc.arg(sent_by)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: SetNotificationResult :exec
UPDATE notifications
SET result = sqlc.arg(result)::jsonb
WHERE id = sqlc.arg(id);

-- name: SetMailTaskNotification :exec
UPDATE mail_tasks
SET notification_id = sqlc.arg(notification_id)::uuid
WHERE id = sqlc.arg(id);

-- A send's queue rows as POST /v1/notifications answers them: queued (any row
-- not failed) and failed (a recipient whose mail did not render).
-- name: CountMailTaskRows :one
SELECT count(*) FILTER (WHERE status <> 'failed') AS queued,
       count(*) FILTER (WHERE status = 'failed')  AS failed
FROM mail_queue
WHERE task_id = sqlc.arg(task_id);

-- A notification type's text on a channel other than e-mail (hub ticket
-- iletisim-servisi 05: inbox), as the Template seed states it with the type.
-- name: UpsertNotificationVariant :one
INSERT INTO notification_variants (type, channel, title, summary, link)
VALUES (sqlc.arg(type)::text, sqlc.arg(channel)::text, sqlc.arg(title)::text,
        sqlc.arg(summary)::text, sqlc.arg(link)::text)
ON CONFLICT (type, channel) DO UPDATE
    SET title      = EXCLUDED.title,
        summary    = EXCLUDED.summary,
        link       = EXCLUDED.link,
        updated_at = now()
RETURNING *;

-- name: DeleteNotificationVariant :exec
DELETE FROM notification_variants
WHERE type = sqlc.arg(type)::text
  AND channel = sqlc.arg(channel)::text;

-- name: GetNotificationVariant :one
SELECT *
FROM notification_variants
WHERE type = sqlc.arg(type)::text
  AND channel = sqlc.arg(channel)::text;

-- name: ListNotificationVariants :many
SELECT *
FROM notification_variants
ORDER BY type, channel;

-- One notification's inbox item for each person, its text rendered once:
-- gone in 90 days, or at expires_at when that is sooner. A person already
-- holding the notification's item is left as they are.
-- name: InsertInboxItems :execrows
INSERT INTO inbox_items (user_id, notification_id, notification_type, title, summary, link, expires_at)
SELECT person, sqlc.arg(notification_id)::uuid, sqlc.arg(notification_type)::text, sqlc.arg(title)::text,
       sqlc.arg(summary)::text, sqlc.narg(link)::text,
       LEAST(now() + interval '90 days', COALESCE(sqlc.narg(expires_at)::timestamptz, 'infinity'::timestamptz))
FROM unnest(sqlc.arg(user_ids)::uuid[]) AS person
ON CONFLICT (notification_id, user_id) DO NOTHING;

-- A page of one person's inbox, newest first: the items after the cursor
-- (created_at, id), archived ones as archived says (exclude, include, only),
-- none past its expiry.
-- name: ListInboxItems :many
SELECT *
FROM inbox_items
WHERE user_id = sqlc.arg(user_id)::uuid
  AND expires_at > now()
  AND CASE sqlc.arg(archived)::text
          WHEN 'include' THEN true
          WHEN 'only' THEN archived_at IS NOT NULL
          ELSE archived_at IS NULL END
  AND (sqlc.narg(before_created_at)::timestamptz IS NULL
      OR (created_at, id) < (sqlc.narg(before_created_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size)::int;

-- A person's unseen and unread items, archived and expired ones left out.
-- name: CountInboxItems :one
SELECT count(*) FILTER (WHERE seen_at IS NULL) AS unseen,
       count(*) FILTER (WHERE read_at IS NULL) AS unread
FROM inbox_items
WHERE user_id = sqlc.arg(user_id)::uuid
  AND archived_at IS NULL
  AND expires_at > now();

-- One of the person's own items, marked: seen; read (seen too); unread;
-- archived. A mark already there keeps its time. Nothing when the item is not
-- theirs, does not exist or has expired.
-- name: MarkInboxItem :one
UPDATE inbox_items
SET seen_at     = CASE WHEN sqlc.arg(mark)::text IN ('seen', 'read') THEN COALESCE(seen_at, now()) ELSE seen_at END,
    read_at     = CASE sqlc.arg(mark)::text
                      WHEN 'read' THEN COALESCE(read_at, now())
                      WHEN 'unread' THEN NULL
                      ELSE read_at END,
    archived_at = CASE WHEN sqlc.arg(mark)::text = 'archived' THEN COALESCE(archived_at, now()) ELSE archived_at END
WHERE id = sqlc.arg(id)::uuid
  AND user_id = sqlc.arg(user_id)::uuid
  AND expires_at > now()
RETURNING *;

-- Every item of the person's inbox (archived ones left out) not yet seen, or
-- with read not yet read, marked so.
-- name: MarkAllInboxItems :execrows
UPDATE inbox_items
SET seen_at = COALESCE(seen_at, now()),
    read_at = CASE WHEN sqlc.arg(read)::boolean THEN COALESCE(read_at, now()) ELSE read_at END
WHERE user_id = sqlc.arg(user_id)::uuid
  AND archived_at IS NULL
  AND expires_at > now()
  AND (seen_at IS NULL OR (sqlc.arg(read)::boolean AND read_at IS NULL));

-- The inbox sweep: at most batch_size items past their expiry.
-- name: DeleteExpiredInboxItems :execrows
DELETE FROM inbox_items
WHERE id IN (SELECT i.id
             FROM inbox_items i
             WHERE i.expires_at <= sqlc.arg(at)::timestamptz
             ORDER BY i.expires_at, i.id
             LIMIT sqlc.arg(batch_size)::int FOR UPDATE SKIP LOCKED);
-- Bounces and the suppression list (hub spec skymail-bounce-isleme; README,
-- "Geri dönen posta ve bastırma listesi").

-- The queue row a delivery status notification names, locked FOR SHARE until
-- the report is recorded: Account erasure locks the person's rows FOR UPDATE
-- first, so a report either waits for it and finds the row cleared, or holds
-- it off until the suppression it writes is there for erasure to delete.
-- name: LockBounceQueueRow :one
SELECT id, channel, status, recipient_email, sent_at
FROM mail_queue
WHERE id = sqlc.arg(id)
    FOR SHARE;

-- Records what a report or a relay's refusal said about a queue row. A report
-- already recorded (its id's hash) records nothing: 0 rows.
-- name: InsertMailBounce :execrows
INSERT INTO mail_bounces (queue_id, source, kind, action, status, smtp_code, exo_as_code, recipient_matched,
                          ndr_id_sha256, received_at)
VALUES (sqlc.arg(queue_id), sqlc.arg(source)::text, sqlc.arg(kind)::text, sqlc.arg(action)::text,
        sqlc.narg(status)::text, sqlc.narg(smtp_code)::int, sqlc.narg(exo_as_code)::text,
        sqlc.arg(recipient_matched)::boolean, sqlc.narg(ndr_id_sha256)::text, sqlc.arg(received_at)::timestamptz)
ON CONFLICT ON CONSTRAINT mail_bounces_ndr_once DO NOTHING;

-- Suppresses the address of a queue row after a permanent failure: the
-- row's own address, read under a lock (FOR SHARE: a row Account erasure is
-- clearing is waited for and then found empty, and nothing is written). A
-- suppressed address is counted again and keeps its first_seen.
-- name: SuppressQueueRowAddress :execrows
INSERT INTO mail_suppressions (address, recipient_sub, source, reason_code, last_queue_id)
SELECT lower(btrim(q.recipient_email)), q.recipient_sub, sqlc.arg(source)::text, sqlc.arg(reason_code)::text, q.id
FROM mail_queue q
WHERE q.id = sqlc.arg(queue_id)
  AND q.channel = 'email'
  AND btrim(q.recipient_email) LIKE '_%@_%'
    FOR SHARE OF q
ON CONFLICT (address) DO UPDATE
    SET last_seen     = now(),
        count         = mail_suppressions.count + 1,
        source        = EXCLUDED.source,
        reason_code   = EXCLUDED.reason_code,
        last_queue_id = EXCLUDED.last_queue_id,
        recipient_sub = COALESCE(EXCLUDED.recipient_sub, mail_suppressions.recipient_sub);

-- Deletes up to batch_size of the oldest bounces older than retention_days
-- (a year): the content sweep's step.
-- name: DeleteOldMailBounces :execrows
DELETE
FROM mail_bounces
WHERE id IN (SELECT b.id
             FROM mail_bounces b
             WHERE b.received_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
             ORDER BY b.received_at
             LIMIT sqlc.arg(batch_size)::int FOR UPDATE SKIP LOCKED);

-- The suppressed among a send's recipients (lower-cased addresses): what
-- queueing writes failed, or sends flagged when the send is account mail.
-- name: GetSuppressedAddresses :many
SELECT address, source, reason_code
FROM mail_suppressions
WHERE address = ANY (sqlc.arg(addresses)::text[]);

-- The key of a send's template and whether it is a system template, which
-- together say whether it is account mail (mailer.AccountMail), and the
-- send's purpose, which says its stream (mailer.LaneOf).
-- name: GetMailTaskTemplateKey :one
SELECT t.key, coalesce(t.system, false)::boolean AS system, mt.purpose
FROM mail_tasks mt
         LEFT JOIN templates t ON t.id = mt.template_id
WHERE mt.id = sqlc.arg(task_id);

-- Account mail went to these suppressed addresses anyway.
-- name: FlagSuppressionsExempt :exec
UPDATE mail_suppressions
SET exempt_flagged_at = now()
WHERE address = ANY (sqlc.arg(addresses)::text[]);

-- Records a relay's permanent refusal of a queue row's address.
-- name: InsertSMTPRefusalBounce :exec
INSERT INTO mail_bounces (queue_id, source, kind, action, status, smtp_code)
VALUES (sqlc.arg(queue_id), 'smtp', 'hard', 'failed', sqlc.narg(status)::text, sqlc.arg(smtp_code)::int);

-- Removes a suppression: the address is mailed again. What it said is
-- returned for the removal's record, never the address.
-- name: DeleteSuppression :one
DELETE
FROM mail_suppressions
WHERE id = sqlc.arg(id)
RETURNING source, reason_code, count, first_seen;

-- name: InsertSuppressionRemoval :exec
INSERT INTO mail_suppression_removals (actor_sub, source, reason_code, count, first_seen)
VALUES (sqlc.arg(actor_sub)::text, sqlc.arg(source)::text, sqlc.arg(reason_code)::text, sqlc.arg(count)::int,
        sqlc.arg(first_seen)::timestamptz);

-- The unsubscribe record of the duyuru stream (ADR 0072; hub ticket
-- abonelik-cikisi 02; README, "Tek tık çıkış ucu").

-- The unsubscribed among a send's recipients (lower-cased addresses) in a
-- stream: what queueing writes failed when the send is in that stream.
-- name: GetUnsubscribedAddresses :many
SELECT address
FROM mail_unsubscribes
WHERE stream = sqlc.arg(stream)::text
  AND address = ANY (sqlc.arg(addresses)::text[]);

-- The e-mail rows among ids, claimed by claimed_by, whose address left the
-- duyuru stream, with their send's template's key and whether it is a system
-- template (mailer.LaneOf: account mail is never in the duyuru stream).
-- name: GetUnsubscribedClaimedRows :many
SELECT q.id, t.key, coalesce(t.system, false)::boolean AS system, mt.purpose
FROM mail_queue q
         JOIN mail_unsubscribes u ON u.address = lower(btrim(q.recipient_email)) AND u.stream = 'duyuru'
         LEFT JOIN mail_tasks mt ON mt.id = q.task_id
         LEFT JOIN templates t ON t.id = mt.template_id
WHERE q.id = ANY (sqlc.arg(ids)::uuid[])
  AND q.channel = 'email'
  AND q.status = 'processing'
  AND q.claimed_by = sqlc.arg(claimed_by)::text;

-- Writes failed the claimed rows whose address left the stream after they
-- were queued: skipped, never sent, nothing tried (attempts stays).
-- name: SkipUnsubscribedClaimedRows :execrows
UPDATE mail_queue
SET status = 'failed',
    error  = sqlc.arg(error)::text
WHERE id = ANY (sqlc.arg(ids)::uuid[])
  AND status = 'processing'
  AND claimed_by = sqlc.arg(claimed_by)::text;

-- The address's unsubscribe from a stream, if it has one.
-- name: GetUnsubscribe :one
SELECT id, address, stream, source, created_at
FROM mail_unsubscribes
WHERE address = sqlc.arg(address)::text
  AND stream = sqlc.arg(stream)::text;

-- Records an unsubscribe; an address already out of the stream keeps its
-- first record (0 rows).
-- name: InsertUnsubscribe :execrows
INSERT INTO mail_unsubscribes (address, stream, source)
VALUES (sqlc.arg(address)::text, sqlc.arg(stream)::text, sqlc.arg(source)::text)
ON CONFLICT (address, stream) DO NOTHING;

-- Removes the address's unsubscribe from a stream: the person resubscribed.
-- What it said is returned for the reactivation's record, never the address.
-- name: DeleteUnsubscribeOf :one
DELETE
FROM mail_unsubscribes
WHERE address = sqlc.arg(address)::text
  AND stream = sqlc.arg(stream)::text
RETURNING stream, source, created_at;

-- The unsubscribe the operator API names by id, locked until it is removed.
-- name: LockUnsubscribe :one
SELECT id, address, stream, source, created_at
FROM mail_unsubscribes
WHERE id = sqlc.arg(id)
    FOR UPDATE;

-- name: DeleteUnsubscribe :execrows
DELETE
FROM mail_unsubscribes
WHERE id = sqlc.arg(id);

-- name: UnsubscribeExists :one
SELECT EXISTS (SELECT 1 FROM mail_unsubscribes WHERE id = sqlc.arg(id))::boolean AS found;

-- name: InsertUnsubscribeReactivation :exec
INSERT INTO mail_unsubscribe_reactivations (actor, actor_sub, reason, stream, source, unsubscribed_at)
VALUES (sqlc.arg(actor)::text, sqlc.narg(actor_sub)::text, sqlc.narg(reason)::text, sqlc.arg(stream)::text,
        sqlc.arg(source)::text, sqlc.arg(unsubscribed_at)::timestamptz);

-- The reactivations, newest first.
-- name: ListUnsubscribeReactivations :many
SELECT id, reactivated_at, actor, actor_sub, reason, stream, source, unsubscribed_at
FROM mail_unsubscribe_reactivations
ORDER BY reactivated_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: CountUnsubscribeReactivations :one
SELECT count(*)
FROM mail_unsubscribe_reactivations;

-- The suppression list and the unsubscribe record as one list, most recent
-- first: reason bounce (mail_suppressions, every stream) or unsubscribed
-- (mail_unsubscribes, its stream). reason, when given, keeps one of them;
-- search keeps the addresses that contain it, case-insensitively.
-- name: ListSuppressionEntries :many
SELECT e.id, e.address, e.reason, e.stream, e.recipient_sub, e.source, e.reason_code, e.first_seen, e.last_seen,
       e.count, e.last_queue_id, e.exempt_flagged_at
FROM (SELECT s.id, s.address, 'bounce'::text AS reason, 'all'::text AS stream, s.recipient_sub, s.source,
             s.reason_code, s.first_seen, s.last_seen, s.count, s.last_queue_id, s.exempt_flagged_at
      FROM mail_suppressions s
      WHERE sqlc.narg(reason)::text IS NULL OR sqlc.narg(reason)::text = 'bounce'
      UNION ALL
      SELECT u.id, u.address, 'unsubscribed'::text, u.stream, NULL::uuid, u.source, ''::text, u.created_at,
             u.created_at, 1, NULL::uuid, NULL::timestamptz
      FROM mail_unsubscribes u
      WHERE sqlc.narg(reason)::text IS NULL OR sqlc.narg(reason)::text = 'unsubscribed') e
WHERE sqlc.narg(search)::text IS NULL
   OR strpos(e.address, lower(sqlc.narg(search)::text)) > 0
ORDER BY e.last_seen DESC, e.address, e.reason
LIMIT sqlc.arg(row_limit)::int OFFSET sqlc.arg(row_offset)::int;

-- name: CountSuppressionEntries :one
SELECT ((SELECT count(*)
        FROM mail_suppressions s
        WHERE (sqlc.narg(reason)::text IS NULL OR sqlc.narg(reason)::text = 'bounce')
          AND (sqlc.narg(search)::text IS NULL OR strpos(s.address, lower(sqlc.narg(search)::text)) > 0))
           + (SELECT count(*)
              FROM mail_unsubscribes u
              WHERE (sqlc.narg(reason)::text IS NULL OR sqlc.narg(reason)::text = 'unsubscribed')
                AND (sqlc.narg(search)::text IS NULL OR strpos(u.address, lower(sqlc.narg(search)::text)) > 0)))::bigint
           AS total;

-- name: GetSuppressionID :one
SELECT id
FROM mail_suppressions
WHERE id = sqlc.arg(id);
