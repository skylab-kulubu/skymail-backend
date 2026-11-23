package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// A service's Idempotency-Key on POST /v1/mail_tasks/single (hub tickets
// forms-posta-core 11, horizontal-scale 28): the rule of POST
// /v1/notifications, optional here. The same key from the same service
// answers the first answer again (Idempotent-Replayed: true) and queues
// nothing; another request with it is 422. Without the header a send is as
// it always was.

var (
	errSingleIdempotencyKeyInvalid = apperrors.New(
		"mail_task.idempotency_key_invalid",
		"An Idempotency-Key is 1 to 255 visible ASCII characters, the same for every retry of one mail.",
		fiber.StatusBadRequest,
	)
	errSingleIdempotencyKeyNotAllowed = apperrors.New(
		"mail_task.idempotency_key_not_allowed",
		"Only a service's token may send an Idempotency-Key.",
		fiber.StatusBadRequest,
	)
	errSingleIdempotencyKeyReused = apperrors.New(
		"mail_task.idempotency_key_reused",
		"This Idempotency-Key was already used for a different mail. Use a new key for a new mail.",
		fiber.StatusUnprocessableEntity,
	)
)

// errIdempotentReplay rolls back a send whose key another request took while
// this one was queueing: the caller answers that request's answer.
var errIdempotentReplay = errors.New("idempotency key already used")

// singleKey is a service's Idempotency-Key on one single mail.
type singleKey struct {
	sentBy string
	key    string
	hash   string
}

// singleIdempotency reads the request's Idempotency-Key: nil without one. A
// person's token may not send one: its send is a person's, and SkyMail keeps
// no person's key.
func singleIdempotency(c fiber.Ctx, service bool, sentBy string, params requests.SendSingleMail) (*singleKey, error) {
	key := c.Get(IdempotencyKeyHeader)
	if key == "" {
		return nil, nil
	}
	if !service {
		return nil, errSingleIdempotencyKeyNotAllowed
	}
	if !validIdempotencyKey(key) {
		return nil, errSingleIdempotencyKeyInvalid
	}
	hash, err := singleRequestSHA256(params)
	if err != nil {
		return nil, err
	}
	return &singleKey{sentBy: sentBy, key: key, hash: hash}, nil
}

// singleRequestSHA256 is the request as it was understood: the same mail
// hashes the same whatever its JSON looked like (map keys are sorted). Its
// attachments are not part of it: a retry carries the file drawn again (a
// PDF holds the time it was made), and the mail is the same; the first
// request's file is the one that goes. Without attachments the digest is
// what it was before them (omitempty): keys stored before attachments came
// answer their retries as before.
func singleRequestSHA256(params requests.SendSingleMail) (string, error) {
	params.Attachments = nil
	canonical, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// replay answers a request whose key its service already used: the first
// answer again for the same request, 422 for another. done is false when the
// key is new.
func (k *singleKey) replay(c fiber.Ctx, db *database.Store) (bool, error) {
	earlier, err := db.GetMailTaskIdempotencyKey(c.Context(), database.GetMailTaskIdempotencyKeyParams{SentBy: k.sentBy, IdempotencyKey: k.key})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if earlier.RequestSha256 != k.hash {
		return true, errSingleIdempotencyKeyReused
	}
	var answer CreatedMailTask
	if err := json.Unmarshal(earlier.Result, &answer); err != nil {
		return true, err
	}
	c.Set("Idempotent-Replayed", "true")
	return true, c.Status(fiber.StatusCreated).JSON(answer)
}

// record writes the key with the send it queued and its answer, in the
// send's transaction; errIdempotentReplay when another request took the key
// first.
func (k *singleKey) record(ctx context.Context, q *database.Queries, taskID uuid.UUID, answer CreatedMailTask) error {
	result, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	_, err = q.InsertMailTaskIdempotencyKey(ctx, database.InsertMailTaskIdempotencyKeyParams{
		SentBy: k.sentBy, IdempotencyKey: k.key, RequestSha256: k.hash, TaskID: taskID, Result: result,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errIdempotentReplay
	}
	return err
}
