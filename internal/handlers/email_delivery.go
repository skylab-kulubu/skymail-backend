package handlers

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
)

// A person's own e-mail delivery (hub ticket rabbitmq-olaylar 10; ADR 0073):
// which of their own addresses mail does not reach, read with their own
// token (middlewares.AuthenticatePerson) at /v1/me/email-delivery. Only the
// caller's own rows: those suppressed with their sub, and those of the
// addresses their token says are theirs (middlewares.PersonAddresses). The
// route takes no address and no sub, so it never tells whether someone
// else's address is suppressed.

// UndeliverableAddress is one of the caller's addresses mail does not reach.
type UndeliverableAddress struct {
	Address string `json:"address" example:"ayse@example.com"`
	// When the address was first suppressed (UTC).
	Since time.Time `json:"since" example:"2026-10-21T09:00:00Z"`
}

// EmailDelivery is the caller's addresses mail does not reach; empty when
// there is none.
type EmailDelivery struct {
	Undeliverable []UndeliverableAddress `json:"undeliverable"`
}

type EmailDeliveryHandler interface {
	Get(c fiber.Ctx) error
}

type emailDeliveryHandlerImpl struct {
	db *database.Store
}

func NewEmailDeliveryHandler(db *database.Store) EmailDeliveryHandler {
	return &emailDeliveryHandlerImpl{db: db}
}

// Get godoc
//
//	@Summary		Which of my addresses mail does not reach
//	@Description	The caller's own addresses on the suppression list: the rows suppressed with their sub, and the rows of the addresses their token says are theirs — Keycloak's email (unless email_verified is false) and school_email — compared lower-cased and trimmed, each once, oldest first. since is when the address was first suppressed. No source, code or id; no parameter: someone else's address cannot be asked about. Only a person's own token issued for SkyMail (aud skymail) may call it; no role is needed. Served with Cache-Control: no-store.
//	@Tags			Inbox
//	@Produce		json
//	@Success		200	{object}	handlers.EmailDelivery
//	@Failure		401	{object}	apperrors.AppError	"server.unauthorized: no live token, or one not issued for SkyMail"
//	@Failure		403	{object}	apperrors.AppError	"auth.person_required: a service's token"
//	@Router			/me/email-delivery [get]
func (h *emailDeliveryHandlerImpl) Get(c fiber.Ctx) error {
	me, err := owner(c)
	if err != nil {
		return err
	}
	addresses := middlewares.PersonAddresses(c)
	if addresses == nil {
		addresses = []string{}
	}
	rows, err := h.db.ListPersonSuppressions(c.Context(), database.ListPersonSuppressionsParams{
		RecipientSub: me,
		Addresses:    addresses,
	})
	if err != nil {
		return err
	}
	out := EmailDelivery{Undeliverable: make([]UndeliverableAddress, 0, len(rows))}
	for _, row := range rows {
		out.Undeliverable = append(out.Undeliverable, UndeliverableAddress{Address: row.Address, Since: row.FirstSeen.UTC()})
	}
	// The count and the request's id only: never an address or the sub.
	log.Info().
		Str("event", "email_delivery_read").
		Str("correlation_id", requestid.FromContext(c)).
		Int("undeliverable", len(out.Undeliverable)).
		Msg("a person read their e-mail delivery")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(out)
}
