package middlewares

import (
	"fmt"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// AudienceMode is what /v1 does with a token whose aud does not name SkyMail's
// client (RFC 9068 §4: a resource server takes only tokens issued for it).
// Keycloak's userinfo, which /v1 asks, proves a token is live but not whom it
// was issued to (core-internal-auth spec §4.4, deviation 2).
type AudienceMode string

const (
	// AudienceOff does not read aud: /v1 as it was before the check.
	AudienceOff AudienceMode = "off"
	// AudienceLog accepts the token and writes one AudienceMissingEvent line
	// per accepted request, so the callers that would break are known before
	// enforce.
	AudienceLog AudienceMode = "log"
	// AudienceEnforce answers 401 and writes the same line.
	AudienceEnforce AudienceMode = "enforce"
)

// AudienceModeEnv is the variable AudienceModeFromEnv reads.
const AudienceModeEnv = "V1_TOKEN_AUDIENCE_MODE"

// AudienceMissingEvent is the event field of the line log and enforce write
// for a token whose aud lacks SkyMail. The line carries only the caller's
// client (azp) and the mode: never the token, a subject, a name, an address
// or an IP, so it can be counted per client from the service's logs.
const AudienceMissingEvent = "v1_token_audience_missing"

// AudienceModeFromEnv reads V1_TOKEN_AUDIENCE_MODE: off (also when unset),
// log or enforce. Any other value is an error that stops startup, so a typo
// cannot quietly leave the check off or turn it on.
func AudienceModeFromEnv(getenv func(string) string) (AudienceMode, error) {
	raw := strings.TrimSpace(getenv(AudienceModeEnv))
	switch mode := AudienceMode(raw); mode {
	case "", AudienceOff:
		return AudienceOff, nil
	case AudienceLog, AudienceEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be off, log or enforce, not %q", AudienceModeEnv, raw)
	}
}

// tokenAudience reads the aud and azp of a token Keycloak's userinfo has just
// accepted. Keycloak checked its signature, issuer and expiry, so the claims
// are Keycloak's; they are read here, not verified again. A token that is not
// a JWT has no aud SkyMail can read, and counts as lacking it.
func tokenAudience(tokenStr, clientID string) (forUs bool, azp string) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(tokenStr, claims); err != nil {
		return false, ""
	}
	azp, _ = claims["azp"].(string)
	audience, err := claims.GetAudience()
	return err == nil && slices.Contains(audience, clientID), azp
}
