package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

type AuthMiddleware interface {
	Authenticate(c fiber.Ctx) error
	RequireAnyPermission(permissions ...string) func(c fiber.Ctx) error
}

type authMiddlewareImpl struct {
	clientID string
	realmURL string
	client   *client.Client
	logger   *zerolog.Logger
}

type userInfo struct {
	ID                string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	ResourceAccess    map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

func NewAuthMiddleware(clientID string, realmURL string) AuthMiddleware {
	logger := log.With().Str("service", "auth").Logger()
	return &authMiddlewareImpl{
		clientID: clientID,
		realmURL: realmURL,
		client:   client.New(),
		logger:   &logger,
	}
}

func (a *authMiddlewareImpl) Authenticate(c fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return apperrors.ErrForbidden
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		return apperrors.ErrForbidden
	}

	tokenStr := authHeader[7:]
	return a.handleKeycloakAuth(c, tokenStr)
}

func (a *authMiddlewareImpl) handleKeycloakAuth(c fiber.Ctx, tokenStr string) error {
	req := a.client.R()

	req.AddHeader("Authorization", "Bearer "+tokenStr)
	req.SetURL(a.realmURL + "/protocol/openid-connect/userinfo")
	req.SetMethod(fiber.MethodGet)

	resp, err := req.Send()
	if err != nil {
		// The caller's token may be perfectly good; we simply could not ask.
		// That is our failure to report, not theirs to be blamed for.
		a.logger.Error().Err(err).Msg("identity provider unreachable")
		return apperrors.ErrServiceUnavailable
	}

	defer resp.Close()

	// The status has to be read before the body. Keycloak answers an expired or
	// revoked token with 401 and an EMPTY body, putting the reason in
	// WWW-Authenticate, so parsing first turns "your session ended" into a JSON
	// error and then into a 500 the operator reads as a broken server.
	switch status := resp.StatusCode(); {
	case status == fiber.StatusUnauthorized, status == fiber.StatusForbidden:
		return apperrors.ErrUnauthorized
	case status < 200 || status > 299:
		a.logger.Error().Int("status", status).Msg("unexpected userinfo status")
		return apperrors.ErrServiceUnavailable
	}

	var info userInfo
	if err := resp.JSON(&info); err != nil {
		// A 2xx that does not carry the claims is genuinely our problem.
		a.logger.Error().Err(err).Msg("userinfo body could not be read")
		return apperrors.ErrStatusInternalServer
	}

	if info.ID == "" {
		return apperrors.ErrForbidden
	}

	roles := info.ResourceAccess[a.clientID].Roles
	if len(roles) == 0 {
		roles = rolesFromJWT(tokenStr, a.clientID)
	}

	if len(roles) == 0 {
		return apperrors.ErrForbidden
	}

	c.Locals("user_id", info.ID)
	c.Locals("roles", roles)
	if name := info.displayName(); name != "" {
		c.Locals("user_name", name)
	}
	if email := strings.TrimSpace(info.Email); email != "" {
		if info.EmailVerified {
			c.Locals("user_email", email)
		} else {
			c.Locals("user_email_unverified", true)
		}
	}
	return c.Next()
}

// displayName is what the caller is called: a person's token carries their
// name, a service account's only its username. Handlers read it as the
// "user_name" local beside "user_id", and the address the token carries as
// "user_email" — only once Keycloak has verified it; an unverified one is
// left out, and "user_email_unverified" says there was one.
func (info userInfo) displayName() string {
	if name := strings.TrimSpace(info.Name); name != "" {
		return name
	}
	return strings.TrimSpace(info.PreferredUsername)
}

func rolesFromJWT(tokenStr, clientID string) []string {
	parser := jwt.NewParser()
	token, _, err := parser.ParseUnverified(tokenStr, jwt.MapClaims{})
	if err != nil {
		return nil
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil
	}
	ra, ok := claims["resource_access"].(map[string]interface{})
	if !ok {
		return nil
	}
	ca, ok := ra[clientID].(map[string]interface{})
	if !ok {
		return nil
	}
	rolesRaw, ok := ca["roles"].([]interface{})
	if !ok {
		return nil
	}
	roles := make([]string, 0, len(rolesRaw))
	for _, r := range rolesRaw {
		if s, ok := r.(string); ok {
			roles = append(roles, s)
		}
	}
	return roles
}

func (a *authMiddlewareImpl) RequireAnyPermission(permissions ...string) func(c fiber.Ctx) error {
	return func(c fiber.Ctx) error {
		roles, ok := c.Locals("roles").([]string)

		if !ok {
			a.logger.Debug().Msg("no roles found in context")
			return apperrors.ErrForbidden
		}

		for _, p := range permissions {
			for _, role := range roles {
				if role == p {
					return c.Next()
				}
			}
		}

		return apperrors.ErrForbidden
	}
}
