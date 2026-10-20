// Package msauth gets Microsoft Entra ID access tokens for SkyMail's own
// application (skymail-posta) with the OAuth 2.0 client credentials grant and
// a certificate (RFC 7523 client assertion): no client secret exists. The
// mail senders (Graph sendMail, SMTP XOAUTH2; hub ticket skymail-bounce-isleme
// 10) and the NDR reader (ticket 04) share it.
//
// The assertion is signed PS256 with the certificate's SHA-256 thumbprint in
// x5t#S256, as Microsoft recommends; if the token endpoint refuses that, RS256
// with the SHA-1 thumbprint in x5t, as the ops wizard's token_al does
// (ops/wizards/skymail-posta-app-kaydi.sh). Tokens are cached per scope and
// fetched again before they expire.
//
// Nothing here logs or returns key material or a token in an error: the
// token endpoint's errors are reduced to the HTTP status, the OAuth error
// code and the AADSTS numbers, never the description.
package msauth

import (
	"context"
	"crypto/rsa"
	"crypto/sha1" // x5t is the SHA-1 thumbprint (RFC 7515 §4.1.7)
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// The scopes SkyMail asks for: Microsoft Graph (sendMail, reading the sender
// mailboxes' inboxes) and Exchange Online (SMTP XOAUTH2).
const (
	ScopeGraph    = "https://graph.microsoft.com/.default"
	ScopeExchange = "https://outlook.office365.com/.default"
)

// DefaultAuthority is the Entra ID endpoint the token requests go to.
const DefaultAuthority = "https://login.microsoftonline.com"

// The environment the credentials are read from (CredentialsFromEnv).
const (
	EnvTenantID   = "GRAPH_TENANT_ID"
	EnvClientID   = "GRAPH_CLIENT_ID"
	EnvClientKey  = "GRAPH_CLIENT_KEY"
	EnvClientCert = "GRAPH_CLIENT_CERT"
)

// The signing algorithms, in the order they are tried.
const (
	AlgPS256 = "PS256"
	AlgRS256 = "RS256"
)

// Credentials are the application's certificate credential: the tenant, the
// application (client) id, the private key and the thumbprints of its
// certificate. The key never leaves this value.
type Credentials struct {
	TenantID string
	ClientID string

	key      *rsa.PrivateKey
	x5t      string // SHA-1 thumbprint, base64url (RS256)
	x5tS256  string // SHA-256 thumbprint, base64url (PS256)
	notAfter time.Time
}

// tenantPattern is a tenant id (a GUID) or one of its domain names.
var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,253}$`)

// NewCredentials reads the private key and the certificate and checks they
// belong together. key and cert are each PEM, or the base64 of a PEM, or the
// base64 of DER (the ops wizard stores the key as the one-line base64 of its
// PKCS#8 PEM). The key is RSA, PKCS#8 or PKCS#1. An error never quotes either
// value.
func NewCredentials(tenantID, clientID, key, cert string) (*Credentials, error) {
	tenantID, clientID = strings.TrimSpace(tenantID), strings.TrimSpace(clientID)
	if !tenantPattern.MatchString(tenantID) {
		return nil, fmt.Errorf("%s must be the tenant's id or domain", EnvTenantID)
	}
	if _, err := uuid.Parse(clientID); err != nil {
		return nil, fmt.Errorf("%s must be the application's (client) id, a GUID", EnvClientID)
	}
	privateKey, err := parseKey(key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvClientKey, err)
	}
	certificate, err := parseCertificate(cert)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvClientCert, err)
	}
	public, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok || !public.Equal(&privateKey.PublicKey) {
		return nil, fmt.Errorf("%s is not the certificate of the key in %s", EnvClientCert, EnvClientKey)
	}
	sum1 := sha1.Sum(certificate.Raw)
	sum256 := sha256.Sum256(certificate.Raw)
	return &Credentials{
		TenantID: tenantID,
		ClientID: clientID,
		key:      privateKey,
		x5t:      base64.RawURLEncoding.EncodeToString(sum1[:]),
		x5tS256:  base64.RawURLEncoding.EncodeToString(sum256[:]),
		notAfter: certificate.NotAfter,
	}, nil
}

// CredentialsFromEnv reads GRAPH_TENANT_ID, GRAPH_CLIENT_ID, GRAPH_CLIENT_KEY
// (a ${{vault…}} reference in Dokploy) and GRAPH_CLIENT_CERT (the public
// certificate). Each is required.
func CredentialsFromEnv(getenv func(string) string) (*Credentials, error) {
	for _, name := range []string{EnvTenantID, EnvClientID, EnvClientKey, EnvClientCert} {
		if strings.TrimSpace(getenv(name)) == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	return NewCredentials(getenv(EnvTenantID), getenv(EnvClientID), getenv(EnvClientKey), getenv(EnvClientCert))
}

// Thumbprint is the certificate's SHA-256 thumbprint as the assertion carries
// it (x5t#S256): public, for logs.
func (c *Credentials) Thumbprint() string { return c.x5tS256 }

// NotAfter is when the certificate expires; Entra refuses the assertion after.
func (c *Credentials) NotAfter() time.Time { return c.notAfter }

// assertionLifetime is how long a client assertion is valid; skew is how far
// before now it is valid from, for a clock behind Entra's.
const (
	assertionLifetime = 5 * time.Minute
	assertionSkew     = 30 * time.Second
)

// Assertion is a signed client assertion for audience (the token endpoint)
// with alg, PS256 or RS256 (RFC 7523 §3; Microsoft identity platform,
// "certificate credentials").
func (c *Credentials) Assertion(alg, audience string, now time.Time) (string, error) {
	var method jwt.SigningMethod
	header := map[string]string{}
	switch alg {
	case AlgPS256:
		method = jwt.SigningMethodPS256
		header["x5t#S256"] = c.x5tS256
	case AlgRS256:
		method = jwt.SigningMethodRS256
		header["x5t"] = c.x5t
	default:
		return "", fmt.Errorf("unknown signing algorithm %q", alg)
	}
	token := jwt.NewWithClaims(method, jwt.MapClaims{
		"aud": audience,
		"iss": c.ClientID,
		"sub": c.ClientID,
		"jti": uuid.NewString(),
		"iat": now.Unix(),
		"nbf": now.Add(-assertionSkew).Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
	})
	for name, value := range header {
		token.Header[name] = value
	}
	return token.SignedString(c.key)
}

// parseKey reads an RSA private key.
func parseKey(value string) (*rsa.PrivateKey, error) {
	der, err := decodeDER(value, "PRIVATE KEY", "RSA PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the key is not an RSA key")
		}
		return rsaKey, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("not an RSA private key (PKCS#8 or PKCS#1)")
}

// parseCertificate reads an X.509 certificate.
func parseCertificate(value string) (*x509.Certificate, error) {
	der, err := decodeDER(value, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.New("not an X.509 certificate")
	}
	return certificate, nil
}

// decodeDER undoes the forms a value may come in: PEM, base64 of PEM, base64
// of DER. The errors name the form, never the value.
func decodeDER(value string, pemTypes ...string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("empty")
	}
	raw := []byte(value)
	if !strings.Contains(value, "-----BEGIN ") {
		compact := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, value)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(compact, "="))
		}
		if err != nil {
			return nil, errors.New("neither PEM nor base64")
		}
		raw = decoded
	}
	if !strings.Contains(string(raw), "-----BEGIN ") {
		return raw, nil
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("a PEM block that does not read")
	}
	for _, want := range pemTypes {
		if block.Type == want {
			return block.Bytes, nil
		}
	}
	return nil, fmt.Errorf("a PEM block of type %q, want %s", block.Type, strings.Join(pemTypes, " or "))
}

// TokenError is the token endpoint's refusal: its HTTP status, OAuth error
// code and AADSTS numbers. Its description is never kept (it can name the
// application and carries trace ids).
type TokenError struct {
	Status int
	Code   string
	AADSTS []int
}

func (e *TokenError) Error() string {
	text := "token endpoint HTTP " + strconv.Itoa(e.Status)
	if e.Code != "" {
		text += " " + e.Code
	}
	for _, n := range e.AADSTS {
		text += " AADSTS" + strconv.Itoa(n)
	}
	return text
}

// refused says the endpoint refused the client's authentication, which the
// other signing algorithm may pass.
func (e *TokenError) refused() bool { return e.Status == 400 || e.Status == 401 }

// Refreshing: a cached token is fetched again once it has refreshBefore left;
// if that fails, the cached token is still used while it has minLeft left.
const (
	refreshBefore = 5 * time.Minute
	minLeft       = 30 * time.Second
	// defaultLifetime is the lifetime of a token whose response gives none.
	defaultLifetime = 5 * time.Minute
)

// TokenSource is one scope's access token, cached for every caller: the
// workers share one. Safe for concurrent use; one fetch at a time.
type TokenSource struct {
	creds     *Credentials
	scope     string
	authority string
	client    *http.Client
	now       func() time.Time

	// fetching is held by the caller fetching a token; others wait on it
	// (or their context) and then find the token it cached.
	fetching chan struct{}

	mu      sync.Mutex
	token   string
	expires time.Time
	// alg is the signing algorithm the token endpoint last accepted; it is
	// tried first.
	alg string
}

// Option changes a TokenSource.
type Option func(*TokenSource)

// WithAuthority sends the token requests to authority instead of
// DefaultAuthority (tests).
func WithAuthority(authority string) Option {
	return func(s *TokenSource) { s.authority = strings.TrimRight(authority, "/") }
}

// WithHTTPClient makes the token requests with client.
func WithHTTPClient(client *http.Client) Option {
	return func(s *TokenSource) { s.client = client }
}

// WithClock reads the time from now (tests).
func WithClock(now func() time.Time) Option {
	return func(s *TokenSource) { s.now = now }
}

// NewTokenSource is scope's token for creds.
func NewTokenSource(creds *Credentials, scope string, opts ...Option) *TokenSource {
	s := &TokenSource{
		creds:     creds,
		scope:     scope,
		authority: DefaultAuthority,
		client:    &http.Client{Timeout: 20 * time.Second},
		now:       time.Now,
		fetching:  make(chan struct{}, 1),
		alg:       AlgPS256,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Scope is the scope the source's tokens are for.
func (s *TokenSource) Scope() string { return s.scope }

// cached returns the cached token if it has more than left to go.
func (s *TokenSource) cached(left time.Duration) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Add(left).Before(s.expires) {
		return s.token, true
	}
	return "", false
}

// Token returns an access token for the scope: the cached one while it has
// more than five minutes left, otherwise a new one. If the new one cannot be
// had, the cached one is returned while it still has 30 seconds.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if token, ok := s.cached(refreshBefore); ok {
		return token, nil
	}
	select {
	case s.fetching <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.fetching }()
	// Another caller may have fetched it while this one waited.
	if token, ok := s.cached(refreshBefore); ok {
		return token, nil
	}
	token, err := s.fetch(ctx)
	if err != nil {
		if old, ok := s.cached(minLeft); ok {
			return old, nil
		}
		return "", err
	}
	return token, nil
}

// Invalidate drops token from the cache when it is the cached one: the
// service it was sent to refused it (a 401, a failed SMTP AUTH), and the next
// Token fetches a new one.
func (s *TokenSource) Invalidate(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != "" && s.token == token {
		s.token, s.expires = "", time.Time{}
	}
}

// fetch asks the token endpoint with the algorithm that last worked, then
// with the other one when the endpoint refused the assertion.
func (s *TokenSource) fetch(ctx context.Context) (string, error) {
	s.mu.Lock()
	first := s.alg
	s.mu.Unlock()
	algs := []string{first}
	for _, alg := range []string{AlgPS256, AlgRS256} {
		if alg != first {
			algs = append(algs, alg)
		}
	}
	var lastErr error
	for _, alg := range algs {
		token, lifetime, err := s.request(ctx, alg)
		if err == nil {
			s.mu.Lock()
			s.token, s.expires, s.alg = token, s.now().Add(lifetime), alg
			s.mu.Unlock()
			return token, nil
		}
		lastErr = err
		var refused *TokenError
		if !errors.As(err, &refused) || !refused.refused() {
			break
		}
	}
	return "", lastErr
}

// tokenResponse is the endpoint's answer, success or error.
type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
	ErrorCodes  []int           `json:"error_codes"`
}

// oauthCode keeps an OAuth error code only if it looks like one.
var oauthCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// request makes one token request signed with alg.
func (s *TokenSource) request(ctx context.Context, alg string) (string, time.Duration, error) {
	endpoint := s.authority + "/" + url.PathEscape(s.creds.TenantID) + "/oauth2/v2.0/token"
	assertion, err := s.creds.Assertion(alg, endpoint, s.now())
	if err != nil {
		return "", 0, fmt.Errorf("sign the client assertion: %w", err)
	}
	form := url.Values{
		"client_id":             {s.creds.ClientID},
		"scope":                 {s.scope},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	var body tokenResponse
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if resp.StatusCode != http.StatusOK {
		e := &TokenError{Status: resp.StatusCode, AADSTS: body.ErrorCodes}
		if oauthCode.MatchString(body.Error) {
			e.Code = body.Error
		}
		return "", 0, e
	}
	if decodeErr != nil || body.AccessToken == "" {
		return "", 0, errors.New("token endpoint HTTP 200 without an access token")
	}
	return body.AccessToken, lifetimeOf(body.ExpiresIn), nil
}

// lifetimeOf reads expires_in, a number of seconds (or a string of one).
func lifetimeOf(raw json.RawMessage) time.Duration {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil || seconds <= 0 {
		return defaultLifetime
	}
	return time.Duration(seconds) * time.Second
}
