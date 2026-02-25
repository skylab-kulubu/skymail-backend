-- name: CreateTemplate :one
INSERT INTO templates (name, html_content, plain_text_content)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetTemplateById :one
SELECT * FROM templates WHERE id = $1;

-- name: GetAllTemplates :many
SELECT * FROM templates;

-- name: UpdateTemplate :one
UPDATE templates
SET name = $2, html_content = $3, plain_text_content = $4,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteTemplate :exec
DELETE FROM templates WHERE id = $1;


-- name: CreateMailingList :one
INSERT INTO mailing_lists (name)
VALUES ($1)
RETURNING *;

-- name: GetMailingListById :one
SELECT * FROM mailing_lists WHERE id = $1;

-- name: GetAllMailingLists :many
SELECT * FROM mailing_lists;

-- name: UpdateMailingList :one
UPDATE mailing_lists
SET name = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteMailingList :exec
DELETE FROM mailing_lists WHERE id = $1;


-- name: AddRecipientToMailingList :exec
WITH recipient AS (
    INSERT INTO recipients (full_name, email)
    VALUES ($2, $3)
    ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name
    RETURNING id
)
INSERT INTO mailing_list_recipients (mail_list_id, recipient_id)
VALUES ($1, recipient.id)
ON CONFLICT DO NOTHING;

-- name: GetRecipientsByMailingListId :many
SELECT r.*
FROM recipients r
JOIN mailing_list_recipients mlr ON r.id = mlr.recipient_id
WHERE mlr.mail_list_id = $1;

-- name: GetRecipients :many
SELECT * FROM recipients;

-- name: GetRecipientByEmail :one
SELECT * FROM recipients WHERE email = $1;

-- name: UpdateRecipient :one
UPDATE recipients
SET full_name = $2, email = $3, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: RemoveRecipientFromMailingListByID :exec
DELETE FROM mailing_list_recipients
WHERE mail_list_id = $1 AND recipient_id = $2;
