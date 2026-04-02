-- name: CreateTemplate :one
INSERT INTO templates (name, html_content, plain_text_content, react_email_content)
VALUES ($1, $2, $3, $4)
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
    html_content        = $3,
    plain_text_content  = $4,
    react_email_content = $5,
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
    error     = NULL,
    updated_at = NOW()
WHERE id = $1;

-- name: SetMailQueueItemFailed :exec
UPDATE mail_queue
SET status    = 'failed',
    mail_id   = NULL,
    error     = $2,
    updated_at = NOW()
WHERE id = $1;
