// Package erasuretoken verifies the bearer token of core's Erasure command
// (ADR-0051; account erasure spec §2.5) locally, against Keycloak's keys.
package erasuretoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The Erasure command's caller and grant (account erasure spec §2.5): core's
// own erasure client, holding SkyMail's erase role. core-erasure has only a
// service account and asks for SkyMail's scope alone, so a token it sends to
// SkyMail carries no other service's erase role.
const (
	CallerClientID = "core-erasure"
	EraseRole      = "skymail:account:erase"
)

var (
	// ErrUnauthorized: no token, or one SkyMail cannot prove Keycloak
	// issued and is still valid — signature, issuer, expiry.
	ErrUnauthorized = errors.New("erasure token not proven")
	// ErrForbidden: a valid token, but not core-erasure's, not for
	// SkyMail, or without the erase role on SkyMail's client.
	ErrForbidden = errors.New("erasure token lacks the erase grant")
	// ErrKeysUnavailable: Keycloak's signing keys could not be read,
	// so no token can be judged now.
	ErrKeysUnavailable = errors.New("erasure token keys unavailable")
)

// Caller is who sent a verified Erasure command.
type Caller struct {
	// The Keycloak subject of core-erasure's service account: the one the
	// account access gate checks, like every caller's.
	Subject string
}

// Config says whose tokens the erase route takes.
type Config struct {
	// The exact iss: the realm URL SkyMail is configured with.
	Issuer string
	// Keycloak's JWKS: <realm URL>/protocol/openid-connect/certs.
	JWKSURL string
	// The client SkyMail's roles are on (skymail): the token's aud must name
	// it, and the erase role must be under resource_access.<it>.
	ResourceClient string
	HTTPClient     *http.Client
	Now            func() time.Time
}

// Verifier checks the erase route's bearer token locally against
// Keycloak's published keys. The rest of SkyMail asks Keycloak's userinfo,
// which proves a token is live but not whom it was issued to; this route has
// to know the token is core-erasure's and meant for SkyMail (spec §2.5).
type Verifier struct {
	resourceClient string
	parser         *jwt.Parser
	keys           *signingKeys
}

const (
	// Keycloak's clock and ours may differ a little.
	leeway = 10 * time.Second
	// An unknown key id makes at most one fetch in this long: a key Keycloak
	// just rotated in is found, a stream of made-up ids fetches nothing.
	signingKeysMinRefresh = 10 * time.Second
	// Keys are read again when older than this, so a key Keycloak dropped
	// stops being trusted.
	signingKeysMaxAge = 10 * time.Minute
	signingKeysFetch  = 5 * time.Second
	signingKeysMaxLen = 1 << 20
)

func NewVerifier(config Config) *Verifier {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: signingKeysFetch}
	}
	return &Verifier{
		resourceClient: config.ResourceClient,
		parser: jwt.NewParser(
			// Keycloak signs with an asymmetric key; an HMAC or unsigned
			// token is never one of its access tokens.
			jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}),
			jwt.WithIssuer(config.Issuer),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(leeway),
			jwt.WithTimeFunc(now),
		),
		keys: &signingKeys{url: config.JWKSURL, client: client, now: now},
	}
}

// Verify checks an Authorization header and returns the caller, or one of
// ErrUnauthorized, ErrForbidden and ErrKeysUnavailable.
func (v *Verifier) Verify(ctx context.Context, authorization string) (Caller, error) {
	scheme, raw, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	raw = strings.TrimSpace(raw)
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return Caller{}, ErrUnauthorized
	}

	claims := jwt.MapClaims{}
	_, err := v.parser.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, ErrUnauthorized
		}
		return v.keys.key(ctx, kid)
	})
	if errors.Is(err, ErrKeysUnavailable) {
		return Caller{}, ErrKeysUnavailable
	}
	if err != nil {
		return Caller{}, ErrUnauthorized
	}
	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return Caller{}, ErrUnauthorized
	}

	if azp, _ := claims["azp"].(string); azp != CallerClientID {
		return Caller{}, ErrForbidden
	}
	audience, err := claims.GetAudience()
	if err != nil || !slices.Contains(audience, v.resourceClient) {
		return Caller{}, ErrForbidden
	}
	if !slices.Contains(clientRoles(claims, v.resourceClient), EraseRole) {
		return Caller{}, ErrForbidden
	}
	return Caller{Subject: subject}, nil
}

// clientRoles reads resource_access.<client>.roles: the roles on that client
// only, never those on the caller's own client or the realm's.
func clientRoles(claims jwt.MapClaims, client string) []string {
	access, _ := claims["resource_access"].(map[string]any)
	entry, _ := access[client].(map[string]any)
	raw, _ := entry["roles"].([]any)
	roles := make([]string, 0, len(raw))
	for _, role := range raw {
		if s, ok := role.(string); ok {
			roles = append(roles, s)
		}
	}
	return roles
}

// signingKeys is Keycloak's JWKS, read when a token names a key it does not
// hold or when it is old.
type signingKeys struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	byID      map[string]any
	fetchedAt time.Time
}

func (s *signingKeys) key(ctx context.Context, kid string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	stale := s.fetchedAt.IsZero() || now.Sub(s.fetchedAt) >= signingKeysMaxAge
	if key, ok := s.byID[kid]; ok && !stale {
		return key, nil
	}
	if !stale && now.Sub(s.fetchedAt) < signingKeysMinRefresh {
		return nil, ErrUnauthorized
	}

	keys, err := s.fetch(ctx)
	if err != nil {
		return nil, ErrKeysUnavailable
	}
	s.byID, s.fetchedAt = keys, now
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, ErrUnauthorized
}

type jsonWebKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (s *signingKeys) fetch(ctx context.Context) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, signingKeysFetch)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS status %d", response.StatusCode)
	}
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, signingKeysMaxLen)).Decode(&set); err != nil {
		return nil, err
	}

	keys := map[string]any{}
	for _, k := range set.Keys {
		// Keycloak also publishes its encryption key; it never signs.
		if k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		if key, err := k.publicKey(); err == nil {
			keys[k.Kid] = key
		}
	}
	return keys, nil
}

func (k jsonWebKey) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errors.New("unknown curve")
		}
		size := (curve.Params().BitSize + 7) / 8
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) > size || len(y) > size {
			return nil, errors.New("bad EC point")
		}
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):1+size], x)
		copy(point[1+2*size-len(y):], y)
		return ecdsa.ParseUncompressedPublicKey(curve, point)
	default:
		return nil, errors.New("unsupported key type")
	}
}
