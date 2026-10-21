package handlers

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// SuppressionsManageRole lets an operator read the suppression list and
// remove an address from it (Keycloak client role on skymail, given to a
// group: hub ticket skymail-bounce-isleme 06).
const SuppressionsManageRole = "skymail:suppressions:manage"

// maxSuppressionsPage bounds one page of the list.
const maxSuppressionsPage = 100

// maxSuppressionSearch bounds the search text: an address is at most 254.
const maxSuppressionSearch = 254

type SuppressionHandler interface {
	List(c fiber.Ctx) error
	Delete(c fiber.Ctx) error
}

type suppressionHandlerImpl struct {
	db *database.Store
}

func NewSuppressionHandler(db *database.Store) SuppressionHandler {
	return &suppressionHandlerImpl{db: db}
}

// Suppression is one suppressed address as the API serves it.
type Suppression struct {
	// What DELETE /suppressions/{id} names it by.
	ID uuid.UUID `json:"id"`
	// Lower-case.
	Address string `json:"address"`
	// The recipient's Keycloak subject, when the mail refused knew it.
	RecipientSub *uuid.UUID `json:"recipient_sub"`
	// dsn: a delivery status notification said the address does not exist;
	// smtp: the relay refused it when the mail was sent.
	Source string `json:"source" enums:"dsn,smtp"`
	// The codes of the last permanent failure, never its text: 5.1.10,
	// 5.4.1 AS(201806281), RCPT TO 550 5.1.1.
	ReasonCode string    `json:"reason_code" example:"5.1.10"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	// How many permanent failures were recorded.
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
//	@Description	Addresses mail is not sent to after one permanent failure (a relay's refusal of the address, or a delivery status notification saying it does not exist), most recently failed first. Account mail (keycloak.*, account.* templates) is still sent to them and flagged. Nothing leaves the list by itself: an operator removes an address, or Account erasure does. q keeps the addresses that contain it, case-insensitively; X-Total-Count counts them.
//	@Tags			Suppressions
//	@Produce		json
//	@Param			q		query		string	false	"Part of an address"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index (at most 100 rows a page)"
//	@Success		200		{array}		handlers.Suppression
//	@Header			200		{integer}	X-Total-Count		"Number of suppressed addresses matching q"
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
	limit, offset := getPaginationParams(c)
	if limit > maxSuppressionsPage {
		limit = maxSuppressionsPage
	}
	rows, err := h.db.ListSuppressions(c.Context(), database.ListSuppressionsParams{
		Search: search, RowLimit: int(limit), RowOffset: int(offset),
	})
	if err != nil {
		return err
	}
	total, err := h.db.CountSuppressions(c.Context(), search)
	if err != nil {
		return err
	}
	items := make([]Suppression, 0, len(rows))
	for _, row := range rows {
		items = append(items, Suppression{
			ID: row.ID, Address: row.Address, RecipientSub: row.RecipientSub, Source: row.Source,
			ReasonCode: row.ReasonCode, FirstSeen: row.FirstSeen, LastSeen: row.LastSeen, Count: row.Count,
			LastQueueID: row.LastQueueID, ExemptFlaggedAt: row.ExemptFlaggedAt,
		})
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(total, 10))
	return c.JSON(items)
}

// Delete godoc
//
//	@Summary		Remove an address from the suppression list
//	@Description	Mail is sent to the address again. The removal is recorded — who, when, and the suppression's source, code, count and first failure — without the address. The address is named by the list's id, so it is never in a URL.
//	@Tags			Suppressions
//	@Param			id	path	string	true	"Suppression ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
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
