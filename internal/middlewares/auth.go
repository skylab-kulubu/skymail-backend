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
	ID             string `json:"sub"`
	ResourceAccess map[string]struct {
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
		return err
	}

	defer resp.Close()

	var info userInfo
	if err := resp.JSON(&info); err != nil {
		return err
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
	return c.Next()
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
