package handlers

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// SuppressionsManageRole lets an operator read the suppression list and
// remove an address from it (Keycloak client role on skymail, given to a
// group: hub ticket skymail-bounce-isleme 06).
const SuppressionsManageRole = "skymail:suppressions:manage"

// UnsubscribesReactivateRole lets an operator reverse an address's own
// unsubscribe, only when the person asks or after a mistake, with a reason
// that is recorded (Keycloak client role on skymail, given to a group: hub
// ticket abonelik-cikisi 10; ADR 0072 decision 7). It also reads the
// reversals.
const UnsubscribesReactivateRole = "skymail:unsubscribes:reactivate"

// The reasons a row of the list has: a permanent failure (every stream) or
// the address's own unsubscribe (its stream).
const (
	SuppressionReasonBounce       = "bounce"
	SuppressionReasonUnsubscribed = "unsubscribed"
)

// The bounds of a reactivation's reason, in characters.
const (
	minReactivationReason = 10
	maxReactivationReason = 500
)

var (
	errUseReactivate = apperrors.New("suppression.use_reactivate",
		"The address unsubscribed itself: only POST /suppressions/{id}/reactivate, with its role and a reason, reverses it.", fiber.StatusConflict)
	errUseDelete = apperrors.New("suppression.use_delete",
		"The address is suppressed after a permanent failure: DELETE /suppressions/{id} removes it.", fiber.StatusConflict)
	errReasonRequired = apperrors.New("suppression.reason_required",
		"A reason of 10 to 500 printable characters is required.", fiber.StatusBadRequest)
	errReasonHasAddress = apperrors.New("suppression.reason_has_address",
		"The reason must not contain an address.", fiber.StatusBadRequest)
)

// maxSuppressionsPage bounds one page of the list.
const maxSuppressionsPage = 100

// maxSuppressionSearch bounds the search text: an address is at most 254.
const maxSuppressionSearch = 254

type SuppressionHandler interface {
	List(c fiber.Ctx) error
	Delete(c fiber.Ctx) error
	Reactivate(c fiber.Ctx) error
	Reactivations(c fiber.Ctx) error
}

type suppressionHandlerImpl struct {
	db *database.Store
}

func NewSuppressionHandler(db *database.Store) SuppressionHandler {
	return &suppressionHandlerImpl{db: db}
}

// Suppression is one row of the list as the API serves it: a suppressed
// address (reason bounce) or an address's own unsubscribe (reason
// unsubscribed).
type Suppression struct {
	// What DELETE /suppressions/{id} (bounce) or POST
	// /suppressions/{id}/reactivate (unsubscribed) names it by; ids of the
	// two never collide (random UUIDs).
	ID uuid.UUID `json:"id"`
	// Lower-case.
	Address string `json:"address"`
	// bounce: a permanent failure, mail of every stream is held (account
	// mail is sent, flagged); unsubscribed: the address left a stream
	// through SkyMail's link, only that stream's mail is held.
	Reason string `json:"reason" enums:"bounce,unsubscribed"`
	// The stream held: all for a bounce, duyuru for an unsubscribe.
	Stream string `json:"stream" enums:"all,duyuru"`
	// The recipient's Keycloak subject, when the mail refused knew it; null
	// for an unsubscribe.
	RecipientSub *uuid.UUID `json:"recipient_sub"`
	// dsn: a delivery status notification said the address does not exist;
	// smtp: the relay refused it when the mail was sent; link: the address
	// unsubscribed through SkyMail's link (the one-click POST or its page).
	Source string `json:"source" enums:"dsn,smtp,link"`
	// The codes of the last permanent failure, never its text: 5.1.10,
	// 5.4.1 AS(201806281), RCPT TO 550 5.1.1. Empty for an unsubscribe.
	ReasonCode string `json:"reason_code" example:"5.1.10"`
	// When it was first recorded; for an unsubscribe, when it was made.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// How many permanent failures were recorded; 1 for an unsubscribe.
	Count int `json:"count"`
	// The queue row of the last one; null once that row is gone.
	LastQueueID *uuid.UUID `json:"last_queue_id"`
	// When account mail (a login code, a verification, a password reset) was
	// last sent to the address anyway; null if never.
	ExemptFlaggedAt *time.Time `json:"exempt_flagged_at"`
}

// List godoc
//
//	@Summary		List the suppression list
//	@Description	One list of two records, most recent first. reason bounce: an address mail is not sent to after one permanent failure (a relay's refusal of the address, or a delivery status notification saying it does not exist), in every stream; account mail (keycloak.*, account.* templates) is still sent to it and flagged. reason unsubscribed: an address that left the duyuru stream through SkyMail's one-click link; mail of the islem stream still goes to it. Nothing leaves the list by itself: an operator removes a bounce, the person (or an operator with skymail:unsubscribes:reactivate, at their request) reverses an unsubscribe, or Account erasure does. reason keeps one of the two; q keeps the addresses that contain it, case-insensitively; X-Total-Count counts them. Needs skymail:suppressions:manage or skymail:unsubscribes:reactivate.
//	@Tags			Suppressions
//	@Produce		json
//	@Param			reason	query		string	false	"Only bounce or only unsubscribed rows"	Enums(bounce,unsubscribed)
//	@Param			q		query		string	false	"Part of an address"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index (at most 100 rows a page)"
//	@Success		200		{array}		handlers.Suppression
//	@Header			200		{integer}	X-Total-Count		"Number of rows matching reason and q"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/suppressions [get]
func (h *suppressionHandlerImpl) List(c fiber.Ctx) error {
	var search *string
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		if len(q) > maxSuppressionSearch {
			return fiber.NewError(fiber.StatusBadRequest, "q is too long")
		}
		search = &q
	}
	var reason *string
	switch r := c.Query("reason"); r {
	case "":
	case SuppressionReasonBounce, SuppressionReasonUnsubscribed:
		reason = &r
	default:
		return fiber.NewError(fiber.StatusBadRequest, "reason must be bounce or unsubscribed")
	}
	limit, offset := getPaginationParams(c)
	if limit > maxSuppressionsPage {
		limit = maxSuppressionsPage
	}
	rows, err := h.db.ListSuppressionEntries(c.Context(), database.ListSuppressionEntriesParams{
		Reason: reason, Search: search, RowLimit: int(limit), RowOffset: int(offset),
	})
	if err != nil {
		return err
	}
	total, err := h.db.CountSuppressionEntries(c.Context(), database.CountSuppressionEntriesParams{Reason: reason, Search: search})
	if err != nil {
		return err
	}
	items := make([]Suppression, 0, len(rows))
	for _, row := range rows {
		items = append(items, Suppression{
			ID: row.ID, Address: row.Address, Reason: row.Reason, Stream: row.Stream, RecipientSub: row.RecipientSub,
			Source: row.Source, ReasonCode: row.ReasonCode, FirstSeen: row.FirstSeen, LastSeen: row.LastSeen,
			Count: row.Count, LastQueueID: row.LastQueueID, ExemptFlaggedAt: row.ExemptFlaggedAt,
		})
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(total, 10))
	return c.JSON(items)
}

// Delete godoc
//
//	@Summary		Remove an address from the suppression list
//	@Description	A bounce row only: mail is sent to the address again. The removal is recorded — who, when, and the suppression's source, code, count and first failure — without the address. The address is named by the list's id, so it is never in a URL. An unsubscribed row is 409 suppression.use_reactivate: the person left the stream themself.
//	@Tags			Suppressions
//	@Param			id	path	string	true	"Suppression ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		409	{object}	apperrors.AppError	"suppression.use_reactivate: an unsubscribe, not a bounce"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/suppressions/{id} [delete]
func (h *suppressionHandlerImpl) Delete(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "id is not a suppression id")
	}
	actor, _ := c.Locals("user_id").(string)
	if actor == "" {
		return apperrors.ErrForbidden
	}
	var removed database.DeleteSuppressionRow
	if err := h.db.InTx(c.Context(), func(q *database.Queries) error {
		removed, err = q.DeleteSuppression(c.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			unsubscribed, existsErr := q.UnsubscribeExists(c.Context(), id)
			if existsErr != nil {
				return existsErr
			}
			if unsubscribed {
				return errUseReactivate
			}
		}
		if err != nil {
			return err
		}
		return q.InsertSuppressionRemoval(c.Context(), database.InsertSuppressionRemovalParams{
			ActorSub: actor, Source: removed.Source, ReasonCode: removed.ReasonCode,
			Count: removed.Count, FirstSeen: removed.FirstSeen,
		})
	}); err != nil {
		return err
	}
	log.Info().Str("suppression_id", id.String()).Str("source", removed.Source).Str("reason", removed.ReasonCode).
		Msg("Suppression removed: the address is mailed again")
	return c.SendStatus(fiber.StatusNoContent)
}

// ReactivateRequest is why an operator reverses an unsubscribe.
type ReactivateRequest struct {
	// 10 to 500 printable characters, without an address (no @): what the
	// person asked, or what went wrong. Recorded with who and when.
	Reason string `json:"reason" example:"Kişi bilgi kutusuna yazıp yeniden abone olmak istedi."`
}

// Reactivate godoc
//
//	@Summary		Reverse an address's unsubscribe
//	@Description	Only when the person asks, or after a mistake (ADR 0072 decision 7): duyuru mail is sent to the address again. Needs skymail:unsubscribes:reactivate. The reason is required (10 to 500 printable characters) and must not contain an address. The reversal is recorded — who, when, the reason, and the record's stream, source and date — without the address. A bounce row is 409 suppression.use_delete.
//	@Tags			Suppressions
//	@Accept			json
//	@Param			id		path	string						true	"Suppression ID (an unsubscribed row)"
//	@Param			body	body	handlers.ReactivateRequest	true	"Why"
//	@Success		204		"No Content"
//	@Failure		400		{object}	apperrors.AppError	"suppression.reason_required, suppression.reason_has_address, or a malformed id"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Failure		409		{object}	apperrors.AppError	"suppression.use_delete: a bounce, not an unsubscribe"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/suppressions/{id}/reactivate [post]
func (h *suppressionHandlerImpl) Reactivate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "id is not a suppression id")
	}
	actor, _ := c.Locals("user_id").(string)
	if actor == "" {
		return apperrors.ErrForbidden
	}
	var body ReactivateRequest
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return errReasonRequired
	}
	reason := strings.TrimSpace(body.Reason)
	if !validReactivationReason(reason) {
		return errReasonRequired
	}
	if strings.Contains(reason, "@") {
		return errReasonHasAddress
	}
	var reactivated database.LockUnsubscribeRow
	if err := h.db.InTx(c.Context(), func(q *database.Queries) error {
		reactivated, err = q.LockUnsubscribe(c.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, bounceErr := q.GetSuppressionID(c.Context(), id); bounceErr == nil {
				return errUseDelete
			}
		}
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(reason), reactivated.Address) {
			return errReasonHasAddress
		}
		if _, err := q.DeleteUnsubscribe(c.Context(), id); err != nil {
			return err
		}
		return q.InsertUnsubscribeReactivation(c.Context(), database.InsertUnsubscribeReactivationParams{
			Actor: database.ReactivatedByOperator, ActorSub: &actor, Reason: &reason,
			Stream: reactivated.Stream, Source: reactivated.Source, UnsubscribedAt: reactivated.CreatedAt,
		})
	}); err != nil {
		return err
	}
	log.Info().Str("suppression_id", id.String()).Str("stream", reactivated.Stream).Str("source", reactivated.Source).
		Msg("Unsubscribe reversed by an operator: the address is mailed again")
	return c.SendStatus(fiber.StatusNoContent)
}

// validReactivationReason says reason is 10 to 500 characters of printable
// text: letters, marks, numbers, punctuation, symbols and spaces, no control
// character or line break.
func validReactivationReason(reason string) bool {
	n := utf8.RuneCountInString(reason)
	if !utf8.ValidString(reason) || n < minReactivationReason || n > maxReactivationReason {
		return false
	}
	for _, r := range reason {
		if r != ' ' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Reactivation is one reversal of an unsubscribe as the API serves it.
type Reactivation struct {
	ID            int64     `json:"id"`
	ReactivatedAt time.Time `json:"reactivated_at"`
	// recipient: the person, on their link's page; operator: the panel.
	Actor string `json:"actor" enums:"recipient,operator"`
	// The operator's Keycloak subject (Silinmiş kullanıcı's after Account
	// erasure); null for the person.
	ActorSub *string `json:"actor_sub"`
	// The operator's reason; null for the person.
	Reason *string `json:"reason"`
	Stream string  `json:"stream" enums:"duyuru"`
	Source string  `json:"source" enums:"link"`
	// When the unsubscribe reversed had been made.
	UnsubscribedAt time.Time `json:"unsubscribed_at"`
}

// Reactivations godoc
//
//	@Summary		List the reversals of unsubscribes
//	@Description	Who reversed an unsubscribe and when — the person themself, or an operator with the reason they gave — newest first, without the address. Needs skymail:unsubscribes:reactivate. X-Total-Count counts them.
//	@Tags			Suppressions
//	@Produce		json
//	@Param			_start	query		int	false	"Start index"
//	@Param			_end	query		int	false	"End index (at most 100 rows a page)"
//	@Success		200		{array}		handlers.Reactivation
//	@Header			200		{integer}	X-Total-Count		"Number of reversals"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/suppressions/reactivations [get]
func (h *suppressionHandlerImpl) Reactivations(c fiber.Ctx) error {
	limit, offset := getPaginationParams(c)
	if limit > maxSuppressionsPage {
		limit = maxSuppressionsPage
	}
	rows, err := h.db.ListUnsubscribeReactivations(c.Context(), database.ListUnsubscribeReactivationsParams{
		RowLimit: int(limit), RowOffset: int(offset),
	})
	if err != nil {
		return err
	}
	total, err := h.db.CountUnsubscribeReactivations(c.Context())
	if err != nil {
		return err
	}
	items := make([]Reactivation, 0, len(rows))
	for _, row := range rows {
		items = append(items, Reactivation{
			ID: row.ID, ReactivatedAt: row.ReactivatedAt, Actor: row.Actor, ActorSub: row.ActorSub, Reason: row.Reason,
			Stream: row.Stream, Source: row.Source, UnsubscribedAt: row.UnsubscribedAt,
		})
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(total, 10))
	return c.JSON(items)
}
