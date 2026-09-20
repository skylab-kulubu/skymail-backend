package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

func AccountAccessGate(reader accessgate.Reader) fiber.Handler {
	return accountAccessGate(reader, &log.Logger)
}

func accountAccessGate(reader accessgate.Reader, logger *zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		if reader == nil {
			return c.Next()
		}

		subject, ok := c.Locals("user_id").(string)
		if !ok || strings.TrimSpace(subject) == "" {
			logAccessDecision(logger, c, accessgate.Unavailable)
			return unavailable(c)
		}

		decision := reader.Check(c.Context(), subject)
		logAccessDecision(logger, c, decision)
		switch decision {
		case accessgate.Allowed:
			return c.Next()
		case accessgate.Blocked:
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return apperrors.ErrUnauthorized
		default:
			return unavailable(c)
		}
	}
}

func unavailable(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderRetryAfter, "1")
	return apperrors.ErrServiceUnavailable
}

func logAccessDecision(logger *zerolog.Logger, c fiber.Ctx, decision accessgate.Decision) {
	event := logger.Debug()
	switch decision {
	case accessgate.Blocked:
		event = logger.Warn()
	case accessgate.Unavailable:
		event = logger.Error()
	}
	event.
		Str("service", "account_access_gate").
		Str("correlation_id", requestid.FromContext(c)).
		Str("decision", string(decision)).
		Msg("account access decision")
}
