package middlewares

import (
	"context"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
)

// TeamLeaders says which Teams a person leads (teams.Core.Led: what core said
// up to a minute ago).
type TeamLeaders interface {
	Led(ctx context.Context, sub string) ([]teams.Team, error)
}

const teamSeatKey = "skymail.team_seat"

// RequireAccessOrTeamLeader is the seat of Team mail's routes (ADR-0067
// addendum of 2026-11-08): a caller with skymail:access goes on as on /v1;
// one without it goes on only when core says they lead at least one Team,
// and then TeamSeat is true for the handler, which keeps them to Team mail.
// Core not answering refuses only these callers, 503
// team_mail.leadership_unavailable with Retry-After; anyone with
// skymail:access never waits for core here. It follows
// AuthenticateTeamSender and the account access gate.
func RequireAccessOrTeamLeader(leaders TeamLeaders) fiber.Handler {
	return func(c fiber.Ctx) error {
		roles, _ := c.Locals("roles").([]string)
		if slices.Contains(roles, AccessRole) {
			return c.Next()
		}
		sub, _ := c.Locals("user_id").(string)
		if leaders == nil || sub == "" {
			return apperrors.ErrForbidden
		}
		led, err := leaders.Led(c.Context(), sub)
		if err != nil {
			log.Warn().Err(err).Str("event", "team_mail_leadership_unavailable").Msg("Team mail: core did not say which Teams the caller leads")
			c.Set(fiber.HeaderCacheControl, "no-store")
			return teams.ErrLeadershipUnavailable
		}
		if len(led) == 0 {
			return apperrors.ErrForbidden
		}
		c.Locals(teamSeatKey, true)
		return c.Next()
	}
}

// TeamSeat says the caller reached a Team mail route without skymail:access,
// as a Leader core named: they may do Team mail and nothing else.
func TeamSeat(c fiber.Ctx) bool {
	seat, _ := c.Locals(teamSeatKey).(bool)
	return seat
}
