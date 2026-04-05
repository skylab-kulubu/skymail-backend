-- name: CreateTemplate :one
INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetTemplateById :one
SELECT *
FROM templates
WHERE id = $1;

-- name: GetAllTemplates :many
SELECT *
FROM templates
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountTemplates :one
SELECT count(*)
FROM templates;

-- name: UpdateTemplate :one
UPDATE templates
SET name                = $2,
    subject             = $3,
    html_content        = $4,
    plain_text_content  = $5,
    react_email_content = $6,
    updated_at          = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteTemplate :exec
DELETE
FROM templates
WHERE id = $1;


-- name: CreateMailingList :one
INSERT INTO mailing_lists (name)
VALUES ($1)
RETURNING *;

-- name: GetMailingListById :one
SELECT *
FROM mailing_lists
WHERE id = $1;

-- name: GetAllMailingLists :many
SELECT *
FROM mailing_lists
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountMailingLists :one
SELECT count(*)
FROM mailing_lists;

-- name: UpdateMailingList :one
UPDATE mailing_lists
SET name       = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteMailingList :exec
DELETE
FROM mailing_lists
WHERE id = $1;


-- name: AddRecipientToMailingList :one
WITH recipient AS (
    INSERT INTO recipients (full_name, email)
        VALUES ($2, $3)
        ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name
        RETURNING id, full_name, email, created_at, updated_at),
     association AS (
         INSERT INTO mailing_list_recipients (mail_list_id, recipient_id)
             SELECT $1, id FROM recipient
             ON CONFLICT DO NOTHING)
SELECT *
FROM recipient;

-- name: GetRecipientsByMailingListId :many
SELECT r.*
FROM recipients r
         JOIN mailing_list_recipients mlr ON r.id = mlr.recipient_id
WHERE mlr.mail_list_id = $1
ORDER BY r.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountRecipientsByMailingListId :one
SELECT count(*)
FROM recipients r
         JOIN mailing_list_recipients mlr ON r.id = mlr.recipient_id
WHERE mlr.mail_list_id = $1;

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
             ORDER BY created_at
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
    error     = $2
WHERE id = $1;

-- name: CreateMailTask :many
WITH inserted_task AS (
    INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables)
        VALUES ($1, $2, $3, $4)
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

-- name: GetAllMailTasks :many
SELECT mt.*,
       t.name  AS template_name,
       ml.name AS mail_list_name
FROM mail_tasks mt
         LEFT JOIN templates t ON mt.template_id = t.id
         LEFT JOIN mailing_lists ml ON mt.mail_list_id = ml.id
ORDER BY mt.created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountMailTasks :one
SELECT count(*)
FROM mail_tasks;

-- name: GetMailQueueItemsByTaskId :many
SELECT id, recipient_full_name, recipient_email, status, error, created_at
FROM mail_queue
WHERE task_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountMailQueueItemsByTaskId :one
SELECT count(*)
FROM mail_queue
WHERE task_id = $1;
