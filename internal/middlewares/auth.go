package middlewares

import (
	jwtware "github.com/gofiber/contrib/v3/jwt"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

type AuthMiddleware interface {
	GetRoles(c fiber.Ctx) error
	RequireAnyPermission(permissions ...string) func(c fiber.Ctx) error
}

type authMiddlewareImpl struct {
	clientID string
	realmURL string
	client   *client.Client
	logger   *zerolog.Logger
}

type userInfo struct {
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

func (a *authMiddlewareImpl) GetRoles(c fiber.Ctx) error {
	user := jwtware.FromContext(c)
	if user == nil {
		return apperrors.ErrForbidden
	}

	req := a.client.R()

	req.AddHeader("Authorization", "Bearer "+user.Raw)
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

	if len(info.ResourceAccess) == 0 {
		return apperrors.ErrForbidden
	}

	if _, ok := info.ResourceAccess[a.clientID]; !ok {
		return apperrors.ErrForbidden
	}

	if len(info.ResourceAccess[a.clientID].Roles) == 0 {
		return apperrors.ErrForbidden
	}

	c.Locals("roles", info.ResourceAccess[a.clientID].Roles)
	return c.Next()
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
