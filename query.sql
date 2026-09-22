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
INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, system)
VALUES (sqlc.arg(key)::text,
        sqlc.arg(name)::text,
        sqlc.arg(subject)::text,
        sqlc.arg(html_content)::text,
        sqlc.arg(plain_text_content)::text,
        sqlc.arg(react_email_content)::text,
        sqlc.arg(system)::boolean)
-- The seed owns a template's structure; an operator owns its subject. The repo
-- seeds the subject once, on insert, and never writes over it again: ADR-0045
-- moved Keycloak's system mail here so a wording change would stop costing a
-- release, and re-seeding is frequent enough that overwriting the subject took
-- that back silently. A subject fix made in the repo therefore does not reach a
-- key that already exists; someone has to make it in SkyMail too.
ON CONFLICT (key) DO UPDATE
    SET name                = EXCLUDED.name,
        html_content        = EXCLUDED.html_content,
        plain_text_content  = EXCLUDED.plain_text_content,
        react_email_content = EXCLUDED.react_email_content,
        system              = EXCLUDED.system,
        archived_at         = NULL,
        archived_by         = NULL,
        updated_at          = NOW()
RETURNING *;

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

-- name: RestoreTemplate :one
UPDATE templates
SET archived_at = NULL,
    archived_by = NULL,
    updated_at = CASE WHEN archived_at IS NULL THEN updated_at ELSE NOW() END
WHERE id = $1
RETURNING *;


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
