package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

type AuthMiddleware interface {
	Authenticate(c fiber.Ctx) error
	RequireAnyPermission(permissions ...string) func(c fiber.Ctx) error
}

type authMiddlewareImpl struct {
	db        *database.Store
	appSecret []byte
	clientID  string
	realmURL  string
	client    *client.Client
	logger    *zerolog.Logger
}

type userInfo struct {
	ID             string `json:"sub"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

func NewAuthMiddleware(db *database.Store, appSecret string, clientID string, realmURL string) AuthMiddleware {
	logger := log.With().Str("service", "auth").Logger()
	return &authMiddlewareImpl{
		db:        db,
		appSecret: []byte(appSecret),
		clientID:  clientID,
		realmURL:  realmURL,
		client:    client.New(),
		logger:    &logger,
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

	// Parse without validation first to check issuer
	parser := jwt.NewParser()
	unverifiedToken, _, err := parser.ParseUnverified(tokenStr, jwt.MapClaims{})
	if err != nil {
		return apperrors.ErrForbidden
	}

	claims, ok := unverifiedToken.Claims.(jwt.MapClaims)
	if !ok {
		return apperrors.ErrForbidden
	}

	iss, _ := claims["iss"].(string)

	if iss == "skymail" {
		return a.handleAppAuth(c, tokenStr)
	}

	return a.handleKeycloakAuth(c, tokenStr)
}

func (a *authMiddlewareImpl) handleAppAuth(c fiber.Ctx, tokenStr string) error {
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		return a.appSecret, nil
	})

	if err != nil || !token.Valid {
		return apperrors.ErrForbidden
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return apperrors.ErrForbidden
	}

	appIDStr, ok := claims["app_id"].(string)
	if !ok {
		return apperrors.ErrForbidden
	}

	tokenVersion, ok := claims["token_version"].(float64)
	if !ok {
		return apperrors.ErrForbidden
	}

	appID, err := uuid.Parse(appIDStr)
	if err != nil {
		return apperrors.ErrForbidden
	}

	currentVersion, err := a.db.GetApplicationTokenVersion(c.Context(), appID)
	if err != nil {
		return apperrors.ErrForbidden
	}

	if int(tokenVersion) != currentVersion {
		return apperrors.ErrForbidden
	}

	c.Locals("user_id", "app_"+appIDStr)
	c.Locals("is_app", true)
	return c.Next()
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
	c.Locals("is_app", false)
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
		isApp, _ := c.Locals("is_app").(bool)
		if isApp {
			return c.Next()
		}

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
