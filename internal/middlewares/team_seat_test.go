package middlewares

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
)

type fixedLeaders []teams.Team

func (l fixedLeaders) Led(context.Context, string) ([]teams.Team, error) { return l, nil }

const seatSub = "a1a1a1a1-1111-4111-8111-111111111111"

func seatToken(t *testing.T) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": seatSub, "azp": "skymail-frontend", "aud": []string{"skymail"}}).
		SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A Leader without skymail:access whose token carries another SkyMail role
// (an approver's) goes on with none: on Team mail's routes they are a Leader
// and nothing more. With skymail:access, the roles stay as they are.
func TestTheSeatWithoutAccessCarriesNoRole(t *testing.T) {
	for name, tc := range map[string]struct {
		roles []string
		want  []string
		seat  bool
	}{
		"approver without access": {[]string{"skymail:mails:approve", "skymail:lists:read"}, []string{}, true},
		"no role":                 {nil, []string{}, true},
		"access":                  {[]string{"skymail:access", "skymail:mails:approve"}, []string{"skymail:access", "skymail:mails:approve"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"sub": seatSub, "resource_access": map[string]any{"skymail": map[string]any{"roles": tc.roles}}})
			realm := userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, string(body))
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			var roles []string
			seat := false
			app.Get("/probe", NewAuthMiddleware("skymail", realm, AudienceOff).AuthenticateTeamSender,
				RequireAccessOrTeamLeader(fixedLeaders{{Path: "/UYELER/ARGE/WEBLAB"}}),
				func(c fiber.Ctx) error {
					roles, _ = c.Locals("roles").([]string)
					seat = TeamSeat(c)
					return c.SendStatus(fiber.StatusNoContent)
				})
			request := httptest.NewRequest(fiber.MethodGet, "/probe", nil)
			request.Header.Set(fiber.HeaderAuthorization, "Bearer "+seatToken(t))
			response, err := app.Test(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != fiber.StatusNoContent || !slices.Equal(roles, tc.want) || seat != tc.seat {
				t.Fatalf("status %d, roles %v, seat %v; want 204, %v, %v", response.StatusCode, roles, seat, tc.want, tc.seat)
			}
		})
	}
}
