package msauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testTenant = "733f9063-0000-4000-8000-000000000001"
	testClient = "90a70433-0000-4000-8000-000000000002"
)

// testPair is a throwaway RSA key and its self-signed certificate, made once:
// 2048 bits keeps the tests quick (production's is 3072).
type testPair struct {
	key     *rsa.PrivateKey
	certDER []byte
}

var (
	pairOnce sync.Once
	pair     testPair
	pairErr  error
)

func keyPair(t *testing.T) testPair {
	t.Helper()
	pairOnce.Do(func() {
		var key *rsa.PrivateKey
		key, pairErr = rsa.GenerateKey(rand.Reader, 2048)
		if pairErr != nil {
			return
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "skymail-posta-test"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
		}
		var der []byte
		der, pairErr = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		pair = testPair{key: key, certDER: der}
	})
	if pairErr != nil {
		t.Fatal(pairErr)
	}
	return pair
}

// keyValue is the key as the ops wizard stores it: the one-line base64 of its
// PKCS#8 PEM.
func (p testPair) keyValue(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(p.key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// certValue is the certificate as GRAPH_CLIENT_CERT holds it: the base64 of
// the .cer (DER) the wizard wrote.
func (p testPair) certValue() string { return base64.StdEncoding.EncodeToString(p.certDER) }

func testCredentials(t *testing.T) *Credentials {
	t.Helper()
	p := keyPair(t)
	creds, err := NewCredentials(testTenant, testClient, p.keyValue(t), p.certValue())
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// The assertion is a JWT the certificate's public key verifies: PS256 with
// x5t#S256, or RS256 with x5t, the claims RFC 7523 and Entra ask for.
func TestAssertionIsSignedByTheCertificatesKey(t *testing.T) {
	p := keyPair(t)
	creds := testCredentials(t)
	audience := DefaultAuthority + "/" + testTenant + "/oauth2/v2.0/token"
	now := time.Now()
	sum1 := sha1.Sum(p.certDER)
	sum256 := sha256.Sum256(p.certDER)

	for _, tc := range []struct {
		alg, header, thumbprint string
		method                  jwt.SigningMethod
	}{
		{AlgPS256, "x5t#S256", base64.RawURLEncoding.EncodeToString(sum256[:]), jwt.SigningMethodPS256},
		{AlgRS256, "x5t", base64.RawURLEncoding.EncodeToString(sum1[:]), jwt.SigningMethodRS256},
	} {
		t.Run(tc.alg, func(t *testing.T) {
			signed, err := creds.Assertion(tc.alg, audience, now)
			if err != nil {
				t.Fatal(err)
			}
			claims := jwt.MapClaims{}
			token, err := jwt.ParseWithClaims(signed, claims, func(token *jwt.Token) (any, error) {
				return &p.key.PublicKey, nil
			}, jwt.WithValidMethods([]string{tc.method.Alg()}), jwt.WithAudience(audience), jwt.WithIssuer(testClient))
			if err != nil {
				t.Fatalf("the assertion does not verify: %v", err)
			}
			if token.Header[tc.header] != tc.thumbprint {
				t.Errorf("header %s = %v, want %s", tc.header, token.Header[tc.header], tc.thumbprint)
			}
			other := map[string]string{"x5t": "x5t#S256", "x5t#S256": "x5t"}[tc.header]
			if _, ok := token.Header[other]; ok {
				t.Errorf("header %s is set too", other)
			}
			if token.Header["typ"] != "JWT" {
				t.Errorf("typ = %v", token.Header["typ"])
			}
			if claims["sub"] != testClient || claims["jti"] == "" || claims["aud"] != audience {
				t.Errorf("claims = %v", claims)
			}
			exp, _ := claims.GetExpirationTime()
			nbf, _ := claims.GetNotBefore()
			if exp == nil || nbf == nil || exp.Sub(now) > 6*time.Minute || now.Sub(nbf.Time) > time.Minute {
				t.Errorf("exp %v nbf %v, want within minutes of now", exp, nbf)
			}
		})
	}

	again, _ := creds.Assertion(AlgPS256, audience, now)
	first, _ := creds.Assertion(AlgPS256, audience, now)
	if again == first {
		t.Error("two assertions share a jti")
	}
}

// The key and the certificate are read in the forms they are kept in, and
// must belong together; an error never quotes them.
func TestNewCredentialsReadsTheKeyAndTheCertificate(t *testing.T) {
	p := keyPair(t)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(p.key)
	pkcs1 := x509.MarshalPKCS1PrivateKey(p.key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.certDER}))

	for name, kc := range map[string][2]string{
		"base64 of PKCS#8 PEM, base64 of DER": {p.keyValue(t), p.certValue()},
		"PEM, PEM":                            {keyPEM, certPEM},
		"base64 of PKCS#1 DER, base64 of PEM": {base64.StdEncoding.EncodeToString(pkcs1), base64.StdEncoding.EncodeToString([]byte(certPEM))},
		"wrapped base64":                      {wrap(p.keyValue(t)), wrap(p.certValue())},
	} {
		t.Run(name, func(t *testing.T) {
			creds, err := NewCredentials(" "+testTenant+" ", testClient, kc[0], kc[1])
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(p.certDER)
			if creds.Thumbprint() != base64.RawURLEncoding.EncodeToString(sum[:]) || creds.TenantID != testTenant {
				t.Errorf("thumbprint %s tenant %q", creds.Thumbprint(), creds.TenantID)
			}
		})
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	otherDER, _ := x509.MarshalPKCS8PrivateKey(otherKey)
	otherValue := base64.StdEncoding.EncodeToString(otherDER)
	for name, tc := range map[string][4]string{
		"another key":         {testTenant, testClient, otherValue, p.certValue()},
		"not a key":           {testTenant, testClient, base64.StdEncoding.EncodeToString([]byte("secret-ish bytes")), p.certValue()},
		"not base64":          {testTenant, testClient, "%%%not-base64%%%", p.certValue()},
		"a key as the cert":   {testTenant, testClient, p.keyValue(t), p.keyValue(t)},
		"client not a GUID":   {testTenant, "skymail-posta", p.keyValue(t), p.certValue()},
		"tenant with a slash": {"evil/../x", testClient, p.keyValue(t), p.certValue()},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewCredentials(tc[0], tc[1], tc[2], tc[3])
			if err == nil {
				t.Fatal("accepted")
			}
			for _, value := range []string{tc[2], tc[3], "secret-ish bytes"} {
				if len(value) > 8 && strings.Contains(err.Error(), value[:8]) && value != tc[1] {
					t.Errorf("the error %q quotes a value", err)
				}
			}
		})
	}

	if _, err := CredentialsFromEnv(func(name string) string {
		return map[string]string{EnvTenantID: testTenant, EnvClientID: testClient, EnvClientKey: p.keyValue(t)}[name]
	}); err == nil || !strings.Contains(err.Error(), EnvClientCert) {
		t.Errorf("a missing %s: %v", EnvClientCert, err)
	}
}

func wrap(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	return b.String() + s
}

// fakeEndpoint is Entra's token endpoint: it verifies each assertion with the
// certificate's key and answers with tokens it numbers, or refuses.
type fakeEndpoint struct {
	t        *testing.T
	server   *httptest.Server
	requests atomic.Int32
	// refuse makes the endpoint answer 401 invalid_client to these
	// algorithms; status, when set, is its answer to every request.
	refuse map[string]bool
	status atomic.Int32
	// now is the time assertions are checked at: the token source's clock.
	now       func() time.Time
	expiresIn int
	scopes    []string
	mu        sync.Mutex
}

func startEndpoint(t *testing.T) *fakeEndpoint {
	t.Helper()
	p := keyPair(t)
	e := &fakeEndpoint{t: t, refuse: map[string]bool{}, expiresIn: 3599, now: time.Now}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := e.requests.Add(1)
		if r.URL.Path != "/"+testTenant+"/oauth2/v2.0/token" || r.ParseForm() != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.scopes = append(e.scopes, r.PostForm.Get("scope"))
		e.mu.Unlock()
		if status := int(e.status.Load()); status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "temporarily_unavailable", "error_description": "AADSTS50000: description naming " + testClient,
				"error_codes": []int{50000},
			})
			return
		}
		assertion := r.PostForm.Get("client_assertion")
		parsed, err := jwt.Parse(assertion, func(token *jwt.Token) (any, error) { return &p.key.PublicKey, nil },
			jwt.WithAudience(e.server.URL+"/"+testTenant+"/oauth2/v2.0/token"), jwt.WithTimeFunc(e.now))
		if err != nil || r.PostForm.Get("client_id") != testClient || r.PostForm.Get("grant_type") != "client_credentials" ||
			r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			t.Errorf("a token request that does not verify: %v %v", err, r.PostForm)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if e.refuse[parsed.Method.Alg()] {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_description": "AADSTS700027: the description is not kept",
				"error_codes": []int{700027},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer", "expires_in": e.expiresIn,
			"access_token": parsed.Method.Alg() + "-token-" + string(rune('0'+n)),
		})
	}))
	t.Cleanup(e.server.Close)
	return e
}

// clock is a settable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A token is fetched once and cached; it is fetched again five minutes
// before it expires, not sooner; a refused one is dropped.
func TestTokenIsCachedAndRefreshedBeforeItExpires(t *testing.T) {
	endpoint := startEndpoint(t)
	c := &clock{now: time.Now()}
	endpoint.now = c.Now
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(endpoint.server.URL), WithClock(c.Now))
	ctx := context.Background()

	first, err := source.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != "PS256-token-1" {
		t.Fatalf("token %q", first)
	}
	c.Add(50 * time.Minute)
	if again, _ := source.Token(ctx); again != first || endpoint.requests.Load() != 1 {
		t.Fatalf("at 50 minutes: %q after %d requests, want the cached token", again, endpoint.requests.Load())
	}
	c.Add(5 * time.Minute) // 4 minutes 59 seconds left
	refreshed, err := source.Token(ctx)
	if err != nil || refreshed == first || endpoint.requests.Load() != 2 {
		t.Fatalf("at 55 minutes: %q %v after %d requests, want a new token", refreshed, err, endpoint.requests.Load())
	}

	source.Invalidate("not the cached one")
	if same, _ := source.Token(ctx); same != refreshed {
		t.Fatal("invalidating another token dropped the cached one")
	}
	source.Invalidate(refreshed)
	if next, _ := source.Token(ctx); next == refreshed || endpoint.requests.Load() != 3 {
		t.Fatalf("after invalidation: %q after %d requests", next, endpoint.requests.Load())
	}
	if got := endpoint.scopes[0]; got != ScopeGraph {
		t.Errorf("scope %q", got)
	}
}

// While the endpoint fails, the cached token serves as long as it has 30
// seconds; then the failure is the caller's, as codes only.
func TestTokenOutlivesAFailingRefreshUntilItExpires(t *testing.T) {
	endpoint := startEndpoint(t)
	c := &clock{now: time.Now()}
	endpoint.now = c.Now
	source := NewTokenSource(testCredentials(t), ScopeExchange, WithAuthority(endpoint.server.URL), WithClock(c.Now))
	ctx := context.Background()
	first, err := source.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.status.Store(http.StatusServiceUnavailable)
	c.Add(57 * time.Minute)
	if got, err := source.Token(ctx); got != first || err != nil {
		t.Fatalf("with 2 minutes left: %q %v, want the cached token", got, err)
	}
	c.Add(150 * time.Second) // 29 seconds left
	_, err = source.Token(ctx)
	if err == nil {
		t.Fatal("an expired token was returned")
	}
	if err.Error() != "token endpoint HTTP 503 temporarily_unavailable AADSTS50000" {
		t.Errorf("error %q", err)
	}
	if strings.Contains(err.Error(), "description") || strings.Contains(err.Error(), testClient) {
		t.Errorf("the error keeps the description: %q", err)
	}
	// A 503 is not a refused assertion: RS256 is not tried for it.
	if endpoint.requests.Load() != 3 {
		t.Errorf("%d requests, want 3", endpoint.requests.Load())
	}
}

// An endpoint that refuses PS256 gets RS256 with x5t, which is then tried
// first.
func TestTokenFallsBackToRS256(t *testing.T) {
	endpoint := startEndpoint(t)
	endpoint.refuse[AlgPS256] = true
	endpoint.expiresIn = 20 // under minLeft: every call fetches, none is reused
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(endpoint.server.URL))
	token, err := source.Token(context.Background())
	if err != nil || !strings.HasPrefix(token, "RS256-") || endpoint.requests.Load() != 2 {
		t.Fatalf("%q %v after %d requests, want an RS256 token after two", token, err, endpoint.requests.Load())
	}
	if _, err := source.Token(context.Background()); err != nil || endpoint.requests.Load() != 3 {
		t.Fatalf("second fetch: %v after %d requests, want RS256 first (3)", err, endpoint.requests.Load())
	}

	endpoint.refuse[AlgRS256] = true
	_, err = source.Token(context.Background())
	if err == nil || err.Error() != "token endpoint HTTP 401 invalid_client AADSTS700027" {
		t.Fatalf("both refused: %v", err)
	}
}

// Workers asking at once make one request.
func TestTokenIsFetchedOnceForConcurrentCallers(t *testing.T) {
	endpoint := startEndpoint(t)
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(endpoint.server.URL))
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], _ = source.Token(context.Background())
		}()
	}
	wg.Wait()
	for _, token := range tokens {
		if token != tokens[0] || token == "" {
			t.Fatalf("tokens %v", tokens)
		}
	}
	if n := endpoint.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestTokenRequestCarriesTheAssertionInTheForm(t *testing.T) {
	// The assertion goes in the body, never the URL.
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": "3599"})
	}))
	t.Cleanup(server.Close)
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(server.URL+"/"))
	if token, err := source.Token(context.Background()); err != nil || token != "t" {
		t.Fatalf("%q %v", token, err)
	}
	if len(query) != 0 {
		t.Errorf("query %v", query)
	}
	if left := time.Until(source.expires); left < 59*time.Minute {
		t.Errorf("a string expires_in read as %v", left)
	}
}

// A token endpoint that gives no expires_in still has its token reused: it is
// taken to last ten minutes, past the five-minute refresh.
func TestATokenWithoutExpiresInIsReused(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t"})
	}))
	t.Cleanup(server.Close)
	c := &clock{now: time.Now()}
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(server.URL), WithClock(c.Now))
	for range 2 {
		if token, err := source.Token(context.Background()); err != nil || token != "t" {
			t.Fatalf("%q %v", token, err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests for two calls, want 1", n)
	}
	c.Add(5*time.Minute + time.Second)
	if _, err := source.Token(context.Background()); err != nil || requests.Load() != 2 {
		t.Fatalf("with under five minutes left: %v after %d requests, want a new one", err, requests.Load())
	}
}

// While a refresh is in flight, a caller whose context is short gets the
// cached token at once instead of waiting for it.
func TestARefreshInFlightDoesNotHoldBackTheCachedToken(t *testing.T) {
	var requests atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 2 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-" + string(rune('0'+n)), "expires_in": 3599})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c := &clock{now: time.Now()}
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(server.URL), WithClock(c.Now))
	if token, err := source.Token(context.Background()); err != nil || token != "token-1" {
		t.Fatalf("%q %v", token, err)
	}
	c.Add(56 * time.Minute) // under five minutes left: refresh

	refreshed := make(chan string, 1)
	go func() {
		token, _ := source.Token(context.Background())
		refreshed <- token
	}()
	<-started
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begun := time.Now()
	token, err := source.Token(short)
	if err != nil || token != "token-1" || time.Since(begun) > 100*time.Millisecond {
		t.Fatalf("during the refresh: %q %v after %v, want the cached token at once", token, err, time.Since(begun))
	}
	close(release)
	if got := <-refreshed; got != "token-2" {
		t.Fatalf("the refresh got %q", got)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

// After a failed refresh the cached token serves for 30 seconds without a
// request per caller; then the refresh is tried again.
func TestAFailedRefreshBacksOff(t *testing.T) {
	endpoint := startEndpoint(t)
	c := &clock{now: time.Now()}
	endpoint.now = c.Now
	source := NewTokenSource(testCredentials(t), ScopeGraph, WithAuthority(endpoint.server.URL), WithClock(c.Now))
	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	endpoint.status.Store(http.StatusServiceUnavailable)
	c.Add(56 * time.Minute)
	for range 3 {
		if got, err := source.Token(context.Background()); got != first || err != nil {
			t.Fatalf("%q %v, want the cached token", got, err)
		}
	}
	if n := endpoint.requests.Load(); n != 2 {
		t.Fatalf("%d requests during the backoff, want 2", n)
	}
	c.Add(31 * time.Second)
	endpoint.status.Store(0)
	if got, err := source.Token(context.Background()); err != nil || got == first || endpoint.requests.Load() != 3 {
		t.Fatalf("after the backoff: %q %v, %d requests", got, err, endpoint.requests.Load())
	}
}
