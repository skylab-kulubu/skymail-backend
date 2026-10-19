package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/erasuretoken"
)

// TrackingWithdrawalStore is Postgres for the withdrawal route.
type TrackingWithdrawalStore interface {
	WithdrawTracking(ctx context.Context, subject string) (database.TrackingWithdrawal, error)
}

// TrackingWithdrawalHandler serves PUT /internal/v1/tracking-withdrawals/{sub}:
// core telling SkyMail that a person turned open and click tracking off on
// my. (hub ticket iletisim-servisi 14c and 16; design note 07 §4.6, §4.7). It
// closes every token of mail to the person and deletes their openings and
// clicks; their old mail's links still redirect. It is idempotent: every
// call that gets through answers 204, a repeat changing nothing. It works
// whether SKYMAIL_TRACKING is on or off.
type TrackingWithdrawalHandler struct {
	store  TrackingWithdrawalStore
	tokens ErasureTokenVerifier
	// The account access gate; nil when ACCOUNT_ACCESS_GATE_MODE is off.
	gate accessgate.Reader
}

// NewTrackingWithdrawalHandler builds the handler. tokens must take core's
// token with SkyMail's withdraw role (erasuretoken.WithdrawCallerClientID,
// erasuretoken.WithdrawRole), never core-erasure's.
func NewTrackingWithdrawalHandler(store TrackingWithdrawalStore, tokens ErasureTokenVerifier, gate accessgate.Reader) TrackingWithdrawalHandler {
	return TrackingWithdrawalHandler{store: store, tokens: tokens, gate: gate}
}

// The fixed codes the route answers with (RFC 7807). None carries a value
// from the request.
const (
	withdrawalInvalidSubject      = "invalid_withdrawal_subject"
	withdrawalUnauthorized        = "withdrawal_unauthorized"
	withdrawalForbidden           = "withdrawal_forbidden"
	withdrawalAccessGateDown      = "access_gate_unavailable"
	withdrawalTokenKeysDown       = "token_keys_unavailable"
	withdrawalStoreUnavailable    = "withdrawal_store_unavailable"
	withdrawalRetryAfter          = 30
	trackingWithdrawalLogEvent    = "tracking_withdrawal"
	trackingWithdrawalRefuseEvent = "tracking_withdrawal_refused"
)

// Withdraw runs the withdrawal. The subject is never logged or echoed: logs
// carry fixed codes and counts only.
func (h TrackingWithdrawalHandler) Withdraw(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	caller, err := h.tokens.Verify(c.Context(), c.Get(fiber.HeaderAuthorization))
	switch {
	case errors.Is(err, erasuretoken.ErrKeysUnavailable):
		return withdrawalUnavailable(c, withdrawalTokenKeysDown)
	case errors.Is(err, erasuretoken.ErrForbidden):
		return withdrawalProblem(c, fiber.StatusForbidden, withdrawalForbidden, "Withdrawal forbidden")
	case err != nil:
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return withdrawalProblem(c, fiber.StatusUnauthorized, withdrawalUnauthorized, "Withdrawal token rejected")
	}
	// The caller's own marker, as on every route.
	if h.gate != nil {
		switch h.gate.Check(c.Context(), caller.Subject) {
		case accessgate.Allowed:
		case accessgate.Blocked:
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return withdrawalProblem(c, fiber.StatusUnauthorized, withdrawalUnauthorized, "Withdrawal token rejected")
		default:
			return withdrawalUnavailable(c, withdrawalAccessGateDown)
		}
	}

	// Keycloak subjects are lower-case hyphenated UUIDs, stored as they
	// come (mail_queue.recipient_sub); another spelling is refused rather
	// than matching no row and answering 204.
	subject := c.Params("sub")
	parsed, err := uuid.Parse(subject)
	if err != nil || parsed.String() != subject || subject == database.DeletedUserSubject {
		return withdrawalProblem(c, fiber.StatusBadRequest, withdrawalInvalidSubject, "Invalid withdrawal subject")
	}

	done, err := h.store.WithdrawTracking(c.Context(), subject)
	if err != nil {
		state := "none"
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			state = pgErr.Code
		}
		log.Error().Str("event", trackingWithdrawalRefuseEvent).Str("code", withdrawalStoreUnavailable).Str("sqlstate", state).
			Msg("tracking withdrawal store failed")
		return withdrawalUnavailable(c, withdrawalStoreUnavailable)
	}
	log.Info().Str("event", trackingWithdrawalLogEvent).Int64("tokens_closed", done.TokensClosed).
		Int64("events_deleted", done.EventsDeleted).Msg("tracking withdrawn")
	return c.SendStatus(fiber.StatusNoContent)
}

func withdrawalProblem(c fiber.Ctx, status int, code, title string) error {
	if status != fiber.StatusServiceUnavailable {
		log.Warn().Str("event", trackingWithdrawalRefuseEvent).Str("code", code).Msg("tracking withdrawal not done")
	}
	raw, err := json.Marshal(struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Code   string `json:"code"`
	}{"about:blank", title, status, code})
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "application/problem+json")
	return c.Status(status).Send(raw)
}

func withdrawalUnavailable(c fiber.Ctx, code string) error {
	c.Set(fiber.HeaderRetryAfter, strconv.Itoa(withdrawalRetryAfter))
	if code != withdrawalStoreUnavailable {
		log.Warn().Str("event", trackingWithdrawalRefuseEvent).Str("code", code).Msg("tracking withdrawal not done")
	}
	return withdrawalProblem(c, fiber.StatusServiceUnavailable, code, "Withdrawal temporarily unavailable")
}
