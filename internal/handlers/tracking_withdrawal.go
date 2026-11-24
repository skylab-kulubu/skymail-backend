package handlers

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skynotify-backend/internal/accessgate"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
	"github.com/skylab-kulubu/skynotify-backend/internal/erasuretoken"
)

// TrackingWithdrawalHandler serves PUT /internal/v1/tracking-withdrawals/{sub}:
// core telling SkyNotify that a person turned open and click tracking off on
// my. (hub ticket iletisim-servisi 14c and 16; design note 07 §4.6, §4.7).
// Since B9 (ADR-0074 addendum of 2026-11-24: every mail is tracked, an
// objection no longer stops it) the route is kept for one release only, so
// a core from before B9 still gets its success: it checks the caller and
// the subject as before, answers 204 and changes nothing. No token is
// closed and no opening or click is deleted. The route then goes.
type TrackingWithdrawalHandler struct {
	tokens ErasureTokenVerifier
	// The account access gate; nil when ACCOUNT_ACCESS_GATE_MODE is off.
	gate accessgate.Reader
}

// NewTrackingWithdrawalHandler builds the handler. tokens must take core's
// token with SkyNotify's withdraw role (erasuretoken.WithdrawCallerClientID,
// erasuretoken.WithdrawRole), never core-erasure's.
func NewTrackingWithdrawalHandler(tokens ErasureTokenVerifier, gate accessgate.Reader) TrackingWithdrawalHandler {
	return TrackingWithdrawalHandler{tokens: tokens, gate: gate}
}

// The fixed codes the route answers with (RFC 7807). None carries a value
// from the request.
const (
	withdrawalInvalidSubject      = "invalid_withdrawal_subject"
	withdrawalUnauthorized        = "withdrawal_unauthorized"
	withdrawalForbidden           = "withdrawal_forbidden"
	withdrawalAccessGateDown      = "access_gate_unavailable"
	withdrawalTokenKeysDown       = "token_keys_unavailable"
	withdrawalRetryAfter          = 30
	trackingWithdrawalLogEvent    = "tracking_withdrawal_ignored"
	trackingWithdrawalRefuseEvent = "tracking_withdrawal_refused"
)

// Withdraw answers the withdrawal and does nothing (B9). The subject is never
// logged or echoed: logs carry fixed codes only.
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

	log.Info().Str("event", trackingWithdrawalLogEvent).
		Msg("tracking withdrawal ignored: every mail is tracked, nothing changed")
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
	log.Warn().Str("event", trackingWithdrawalRefuseEvent).Str("code", code).Msg("tracking withdrawal not done")
	return withdrawalProblem(c, fiber.StatusServiceUnavailable, code, "Withdrawal temporarily unavailable")
}
