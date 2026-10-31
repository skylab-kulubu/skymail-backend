package middlewares

import (
	"slices"
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
	// AuthenticatePerson is the person's own routes' check (/v1/me): a
	// person's token issued for SkyMail, no role (see its doc).
	AuthenticatePerson(c fiber.Ctx) error
	RequireAnyPermission(permissions ...string) func(c fiber.Ctx) error
}

type authMiddlewareImpl struct {
	clientID string
	realmURL string
	audience AudienceMode
	client   *client.Client
	logger   *zerolog.Logger
}

type userInfo struct {
	ID                string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	// Nil when userinfo does not say (Keycloak's email scope says).
	EmailVerified *bool `json:"email_verified"`
	// The school address, when a mapper puts the user attribute schoolEmail
	// there (Keycloak's profile scope in production).
	SchoolEmail    string `json:"school_email"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

// NewAuthMiddleware authenticates /v1 with Keycloak's userinfo, reads the
// roles on clientID, and treats a token whose aud does not name clientID as
// audience says (V1_TOKEN_AUDIENCE_MODE).
func NewAuthMiddleware(clientID string, realmURL string, audience AudienceMode) AuthMiddleware {
	return newAuthMiddleware(clientID, realmURL, audience, &log.Logger)
}

func newAuthMiddleware(clientID string, realmURL string, audience AudienceMode, base *zerolog.Logger) *authMiddlewareImpl {
	logger := base.With().Str("service", "auth").Logger()
	return &authMiddlewareImpl{
		clientID: clientID,
		realmURL: realmURL,
		audience: audience,
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
	info, err := a.userinfo(tokenStr)
	if err != nil {
		return err
	}

	forUs, azp := true, ""
	if a.audience == AudienceLog || a.audience == AudienceEnforce {
		forUs, azp = tokenAudience(tokenStr, a.clientID)
	}
	if !forUs && a.audience == AudienceEnforce {
		a.logMissingAudience(azp)
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return apperrors.ErrUnauthorized
	}

	roles := info.ResourceAccess[a.clientID].Roles
	if len(roles) == 0 {
		roles = rolesFromJWT(tokenStr, a.clientID)
	}

	if len(roles) == 0 {
		return apperrors.ErrForbidden
	}

	if !forUs {
		a.logMissingAudience(azp)
	}

	c.Locals("user_id", info.ID)
	c.Locals(acceptedTokenIdentityKey, acceptedIdentity(tokenStr))
	c.Locals("roles", roles)
	if name := info.displayName(); name != "" {
		c.Locals("user_name", name)
	}
	if email := strings.TrimSpace(info.Email); email != "" {
		if info.EmailVerified != nil && *info.EmailVerified {
			c.Locals("user_email", email)
		} else {
			c.Locals("user_email_unverified", true)
		}
	}
	return c.Next()
}

var errPersonRequired = apperrors.New(
	"auth.person_required",
	"Only a person's own token may call this; a service's token (client credentials) has no inbox.",
	fiber.StatusForbidden,
)

// AuthenticatePerson authenticates a person's own routes (/v1/me: the inbox,
// hub ticket iletisim-servisi 05; ADR 0070). Keycloak's userinfo must accept
// the bearer, as on /v1. Then, whatever V1_TOKEN_AUDIENCE_MODE says, the
// token's aud must name SkyMail's client — these routes ask for no role, so
// the audience is what keeps a token issued for another service out (401) —
// and it must be a person's: a service account's token (client_id = azp)
// gets 403 auth.person_required, and so does a token whose sub is not the one
// userinfo accepted. The sub is kept ("user_id"), and the person's own
// addresses (PersonAddresses); roles are not read.
func (a *authMiddlewareImpl) AuthenticatePerson(c fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") || len(authHeader) == len("Bearer ") {
		return apperrors.ErrUnauthorized
	}
	tokenStr := authHeader[len("Bearer "):]
	info, err := a.userinfo(tokenStr)
	if err != nil {
		return err
	}
	if forUs, azp := tokenAudience(tokenStr, a.clientID); !forUs {
		a.logger.Warn().
			Str("event", PersonAudienceMissingEvent).
			Str("azp", nonEmpty(azp)).
			Msg("a person route's token aud does not name SkyMail")
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return apperrors.ErrUnauthorized
	}
	identity := acceptedIdentity(tokenStr)
	if identity.subject != info.ID || identity.clientID != "" {
		return errPersonRequired
	}
	c.Locals("user_id", info.ID)
	c.Locals(acceptedTokenIdentityKey, identity)
	c.Locals(personAddressesKey, info.ownAddresses(tokenStr))
	return c.Next()
}

const personAddressesKey = "person_addresses"

// PersonAddresses is what AuthenticatePerson found the caller's own
// addresses to be (GET /v1/me/email-delivery; hub ticket rabbitmq-olaylar
// 10): userinfo's email, only when userinfo says Keycloak has verified it
// (email_verified true; unsaid is not verified, as on /v1), and the school
// address, school_email, from
// userinfo or else from the token Keycloak accepted. Trimmed, lower-cased as
// the suppression list keys an address, each once. Nil when there is none,
// or on a route AuthenticatePerson did not run for.
func PersonAddresses(c fiber.Ctx) []string {
	addresses, _ := c.Locals(personAddressesKey).([]string)
	return addresses
}

func (info userInfo) ownAddresses(tokenStr string) []string {
	var addresses []string
	add := func(address string) {
		address = strings.ToLower(strings.TrimSpace(address))
		if address != "" && !slices.Contains(addresses, address) {
			addresses = append(addresses, address)
		}
	}
	if info.EmailVerified != nil && *info.EmailVerified {
		add(info.Email)
	}
	school := info.SchoolEmail
	if strings.TrimSpace(school) == "" {
		school = stringClaim(tokenStr, "school_email")
	}
	add(school)
	return addresses
}

// stringClaim is a string claim of a token userinfo has accepted; "" when
// the token does not carry it as a string.
func stringClaim(tokenStr, name string) string {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(tokenStr, claims); err != nil {
		return ""
	}
	value, _ := claims[name].(string)
	return value
}

// PersonAudienceMissingEvent is the event of the line AuthenticatePerson
// writes for a token whose aud lacks SkyMail: the caller's client (azp) and
// nothing else, as AudienceMissingEvent.
const PersonAudienceMissingEvent = "person_token_audience_missing"

func nonEmpty(azp string) string {
	if azp == "" {
		return "none"
	}
	return azp
}

// userinfo asks Keycloak whether the bearer is live and whose it is.
func (a *authMiddlewareImpl) userinfo(tokenStr string) (userInfo, error) {
	req := a.client.R()

	req.AddHeader("Authorization", "Bearer "+tokenStr)
	req.SetURL(a.realmURL + "/protocol/openid-connect/userinfo")
	req.SetMethod(fiber.MethodGet)

	resp, err := req.Send()
	if err != nil {
		// The caller's token may be perfectly good; we simply could not ask.
		// That is our failure to report, not theirs to be blamed for.
		a.logger.Error().Err(err).Msg("identity provider unreachable")
		return userInfo{}, apperrors.ErrServiceUnavailable
	}

	defer resp.Close()

	// The status has to be read before the body. Keycloak answers an expired or
	// revoked token with 401 and an EMPTY body, putting the reason in
	// WWW-Authenticate, so parsing first turns "your session ended" into a JSON
	// error and then into a 500 the operator reads as a broken server.
	switch status := resp.StatusCode(); {
	case status == fiber.StatusUnauthorized, status == fiber.StatusForbidden:
		return userInfo{}, apperrors.ErrUnauthorized
	case status < 200 || status > 299:
		a.logger.Error().Int("status", status).Msg("unexpected userinfo status")
		return userInfo{}, apperrors.ErrServiceUnavailable
	}

	var info userInfo
	if err := resp.JSON(&info); err != nil {
		// A 2xx that does not carry the claims is genuinely our problem.
		a.logger.Error().Err(err).Msg("userinfo body could not be read")
		return userInfo{}, apperrors.ErrStatusInternalServer
	}

	if info.ID == "" {
		return userInfo{}, apperrors.ErrForbidden
	}
	return info, nil
}

// logMissingAudience writes the line a wizard counts per client: azp and the
// mode, nothing else (AudienceMissingEvent).
func (a *authMiddlewareImpl) logMissingAudience(azp string) {
	if azp == "" {
		azp = "none"
	}
	a.logger.Warn().
		Str("event", AudienceMissingEvent).
		Str("azp", azp).
		Str("mode", string(a.audience)).
		Msg("v1 token aud does not name SkyMail")
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
