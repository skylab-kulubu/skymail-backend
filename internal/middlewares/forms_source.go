package middlewares

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

// FormsSourceConfig applies only to creating a Forms audience snapshot. It
// does not change the authentication or audience policy of any existing API.
type FormsSourceConfig struct {
	ServiceSub    string
	ClientID      string
	Issuer        string
	Audience      string
	MaxRecipients int
}

func (c FormsSourceConfig) Enabled() bool { return c.ServiceSub != "" && c.MaxRecipients > 0 }

func FormsSourceConfigFromEnv(getenv func(string) string, issuer, audience string) (FormsSourceConfig, error) {
	c := FormsSourceConfig{Issuer: issuer, Audience: audience, ClientID: strings.TrimSpace(getenv("SKYMAIL_FORMS_SOURCE_CLIENT_ID"))}
	if c.ClientID == "" {
		c.ClientID = "forms"
	}
	c.ServiceSub = strings.TrimSpace(getenv("SKYMAIL_FORMS_SOURCE_SERVICE_SUB"))
	if c.ServiceSub != "" {
		id, err := uuid.Parse(c.ServiceSub)
		if err != nil || id == uuid.Nil || id.String() != c.ServiceSub {
			return c, fmt.Errorf("SKYMAIL_FORMS_SOURCE_SERVICE_SUB must be a canonical nonzero UUID")
		}
	}
	if raw := strings.TrimSpace(getenv("SKYMAIL_FORMS_SOURCE_MAX_RECIPIENTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("SKYMAIL_FORMS_SOURCE_MAX_RECIPIENTS must be a positive whole number")
		}
		c.MaxRecipients = n
	}
	return c, nil
}

// acceptedTokenIdentity is read only after userinfo accepts this exact bearer.
// No token or claims supplied by an HTTP body/header are stored here.
type acceptedTokenIdentity struct {
	subject, issuer, azp, clientID string
	audience                       []string
}

const acceptedTokenIdentityKey = "skymail.accepted_token_identity"

func acceptedIdentity(token string) acceptedTokenIdentity {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return acceptedTokenIdentity{}
	}
	identity := acceptedTokenIdentity{}
	identity.subject, _ = claims["sub"].(string)
	identity.issuer, _ = claims["iss"].(string)
	identity.azp, _ = claims["azp"].(string)
	identity.clientID, _ = claims["client_id"].(string)
	identity.audience, _ = claims.GetAudience()
	return identity
}

func RequireFormsSourceService(config FormsSourceConfig) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !config.Enabled() {
			return apperrors.New("form_source.unavailable", "Forms source preparation is unavailable.", fiber.StatusServiceUnavailable)
		}
		identity, ok := c.Locals(acceptedTokenIdentityKey).(acceptedTokenIdentity)
		sub, _ := c.Locals("user_id").(string)
		if !ok || identity.subject != sub || sub != config.ServiceSub || identity.issuer != config.Issuer ||
			identity.azp != config.ClientID || identity.clientID != config.ClientID || !slices.Contains(identity.audience, config.Audience) {
			return apperrors.ErrForbidden
		}
		return c.Next()
	}
}
