package middlewares

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

var errServiceAccountRequired = apperrors.New(
	"auth.service_account_required",
	"Only a service's own token (client credentials) may call this; a person's token may not.",
	fiber.StatusForbidden,
)

// RequireServiceAccount lets through only a service account's token: one
// Keycloak issued by the client credentials grant, whose client_id claim
// names the client it was issued to (azp) and whose sub is the one userinfo
// accepted. A person's token carries no client_id and gets 403
// auth.service_account_required, whatever its roles (POST /v1/notifications,
// hub ticket iletisim-servisi 03: "kişi token'ı giremez"). It reads the
// token Authenticate accepted, so it runs after it.
func RequireServiceAccount() fiber.Handler {
	return func(c fiber.Ctx) error {
		identity, ok := c.Locals(acceptedTokenIdentityKey).(acceptedTokenIdentity)
		sub, _ := c.Locals("user_id").(string)
		if !ok || sub == "" || identity.subject != sub || identity.clientID == "" || identity.clientID != identity.azp {
			return errServiceAccountRequired
		}
		return c.Next()
	}
}
