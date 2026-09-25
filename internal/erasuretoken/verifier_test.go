package erasuretoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testIssuer = "https://e.yildizskylab.com/realms/e-skylab"

// keycloakKeys serves a JWKS the way Keycloak does: signing keys and an
// encryption key, which is never used to verify.
type keycloakKeys struct {
	mu      sync.Mutex
	keys    []map[string]any
	fetches atomic.Int32
	fail    atomic.Bool
}

func (k *keycloakKeys) add(kid string, public any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch key := public.(type) {
	case *rsa.PublicKey:
		k.keys = append(k.keys, map[string]any{
			"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		})
	case *ecdsa.PublicKey:
		raw, err := key.Bytes()
		if err != nil {
			panic(err)
		}
		size := (len(raw) - 1) / 2
		k.keys = append(k.keys, map[string]any{
			"kid": kid, "kty": "EC", "alg": "ES256", "use": "sig", "crv": "P-256",
			"x": base64.RawURLEncoding.EncodeToString(raw[1 : 1+size]),
			"y": base64.RawURLEncoding.EncodeToString(raw[1+size:]),
		})
	}
}

func (k *keycloakKeys) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	k.fetches.Add(1)
	if k.fail.Load() {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": k.keys})
}

type tokenWorld struct {
	t        *testing.T
	key      *rsa.PrivateKey
	jwks     *keycloakKeys
	now      time.Time
	verifier *Verifier
}

func newTokenWorld(t *testing.T) *tokenWorld {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := &keycloakKeys{}
	encryption, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks.add("sig-1", &key.PublicKey)
	jwks.add("enc-1", &encryption.PublicKey)
	jwks.keys[1]["use"] = "enc"
	jwks.keys[1]["alg"] = "RSA-OAEP"
	server := httptest.NewServer(jwks)
	t.Cleanup(server.Close)

	w := &tokenWorld{t: t, key: key, jwks: jwks, now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	w.verifier = NewVerifier(Config{
		Issuer:         testIssuer,
		JWKSURL:        server.URL,
		ResourceClient: "skymail",
		Now:            func() time.Time { return w.now },
	})
	return w
}

// coreErasureClaims is what Keycloak puts in core-erasure's client
// credentials token for scope account-erase-skymail.
func (w *tokenWorld) coreErasureClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": testIssuer,
		"sub": "8d4f2c1e-0000-4000-8000-00000000c0re",
		"azp": "core-erasure",
		"aud": []any{"skymail"},
		"exp": w.now.Add(5 * time.Minute).Unix(),
		"iat": w.now.Add(-time.Second).Unix(),
		"typ": "Bearer",
		"resource_access": map[string]any{
			"skymail": map[string]any{"roles": []any{"skymail:account:erase"}},
		},
	}
}

func (w *tokenWorld) sign(claims jwt.MapClaims, kid string, key any, method jwt.SigningMethod) string {
	w.t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		w.t.Fatal(err)
	}
	return signed
}

func (w *tokenWorld) bearer(claims jwt.MapClaims) string {
	return "Bearer " + w.sign(claims, "sig-1", w.key, jwt.SigningMethodRS256)
}

func (w *tokenWorld) verify(authorization string) (Caller, error) {
	return w.verifier.Verify(context.Background(), authorization)
}

func TestErasureTokenFromCoreErasureIsAccepted(t *testing.T) {
	w := newTokenWorld(t)
	caller, err := w.verify(w.bearer(w.coreErasureClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if caller.Subject != "8d4f2c1e-0000-4000-8000-00000000c0re" {
		t.Errorf("caller = %+v", caller)
	}

	// A lone audience is a string; the scheme is case-insensitive.
	claims := w.coreErasureClaims()
	claims["aud"] = "skymail"
	if _, err := w.verify("bearer " + w.sign(claims, "sig-1", w.key, jwt.SigningMethodRS256)); err != nil {
		t.Errorf("string audience: %v", err)
	}
	// Keys are fetched once and kept.
	if n := w.jwks.fetches.Load(); n != 1 {
		t.Errorf("JWKS fetched %d times", n)
	}
}

func TestErasureTokenThatIsNotProvenIsUnauthorized(t *testing.T) {
	w := newTokenWorld(t)
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	expired := w.coreErasureClaims()
	expired["exp"] = w.now.Add(-time.Minute).Unix()
	noExpiry := w.coreErasureClaims()
	delete(noExpiry, "exp")
	otherIssuer := w.coreErasureClaims()
	otherIssuer["iss"] = testIssuer + "/"
	notYet := w.coreErasureClaims()
	notYet["nbf"] = w.now.Add(time.Hour).Unix()

	for name, authorization := range map[string]string{
		"no header":            "",
		"not bearer":           "Basic Y29yZTpzZWNyZXQ=",
		"empty bearer":         "Bearer ",
		"not a JWT":            "Bearer not-a-token",
		"signed by a stranger": "Bearer " + w.sign(w.coreErasureClaims(), "sig-1", stranger, jwt.SigningMethodRS256),
		"expired":              w.bearer(expired),
		"no expiry":            w.bearer(noExpiry),
		"another issuer":       w.bearer(otherIssuer),
		"not yet valid":        w.bearer(notYet),
		"no key id":            "Bearer " + w.sign(w.coreErasureClaims(), "", w.key, jwt.SigningMethodRS256),
		"HMAC":                 "Bearer " + w.sign(w.coreErasureClaims(), "sig-1", []byte("shared"), jwt.SigningMethodHS256),
		"unsigned":             "Bearer " + w.sign(w.coreErasureClaims(), "sig-1", jwt.UnsafeAllowNoneSignatureType, jwt.SigningMethodNone),
		"the encryption key":   "Bearer " + w.sign(w.coreErasureClaims(), "enc-1", w.key, jwt.SigningMethodRS256),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.verify(authorization)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v, want unauthorized", err)
			}
		})
	}
}

func TestErasureTokenWithoutTheEraseGrantIsForbidden(t *testing.T) {
	w := newTokenWorld(t)
	for name, change := range map[string]func(jwt.MapClaims){
		"another caller":  func(c jwt.MapClaims) { c["azp"] = "core" },
		"no caller":       func(c jwt.MapClaims) { delete(c, "azp") },
		"not for SkyMail": func(c jwt.MapClaims) { c["aud"] = []any{"skycms", "forms"} },
		"no audience":     func(c jwt.MapClaims) { delete(c, "aud") },
		"no erase role": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"skymail": map[string]any{"roles": []any{"skymail:access"}}}
		},
		"role on the caller": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"core-erasure": map[string]any{"roles": []any{"skymail:account:erase"}}}
		},
		"another service's role": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"skycms": map[string]any{"roles": []any{"cms:account:erase"}}}
		},
		"no resource access": func(c jwt.MapClaims) { delete(c, "resource_access") },
		"realm role only": func(c jwt.MapClaims) {
			delete(c, "resource_access")
			c["realm_access"] = map[string]any{"roles": []any{"skymail:account:erase"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			claims := w.coreErasureClaims()
			change(claims)
			_, err := w.verify(w.bearer(claims))
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want forbidden", err)
			}
		})
	}
}

func TestErasureTokenKeysFollowKeycloakRotationWithoutHammeringIt(t *testing.T) {
	w := newTokenWorld(t)
	if _, err := w.verify(w.bearer(w.coreErasureClaims())); err != nil {
		t.Fatal(err)
	}

	// An unknown key id makes one fetch; another soon after makes none.
	rotated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.now = w.now.Add(time.Minute)
	signedByNew := "Bearer " + w.sign(w.coreErasureClaims(), "sig-2", rotated, jwt.SigningMethodES256)
	if _, err := w.verify(signedByNew); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown key: err = %v", err)
	}
	if _, err := w.verify(signedByNew); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown key again: err = %v", err)
	}
	if n := w.jwks.fetches.Load(); n != 2 {
		t.Fatalf("JWKS fetched %d times, want 2", n)
	}

	// Keycloak publishes the new key; once the pause is over it is found.
	w.jwks.add("sig-2", &rotated.PublicKey)
	w.now = w.now.Add(time.Minute)
	claims := w.coreErasureClaims()
	if _, err := w.verify("Bearer " + w.sign(claims, "sig-2", rotated, jwt.SigningMethodES256)); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestErasureTokenKeysUnreachableIsUnavailable(t *testing.T) {
	w := newTokenWorld(t)
	w.jwks.fail.Store(true)
	if _, err := w.verify(w.bearer(w.coreErasureClaims())); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("err = %v, want keys unavailable", err)
	}
	// Once they are back, the same token is accepted.
	w.jwks.fail.Store(false)
	if _, err := w.verify(w.bearer(w.coreErasureClaims())); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestErasureTokenKeysAreRefreshedWhenOld(t *testing.T) {
	w := newTokenWorld(t)
	if _, err := w.verify(w.bearer(w.coreErasureClaims())); err != nil {
		t.Fatal(err)
	}
	// Keycloak drops the key; after the cache ages out it is no longer trusted.
	w.jwks.mu.Lock()
	w.jwks.keys = w.jwks.keys[1:]
	w.jwks.mu.Unlock()
	w.now = w.now.Add(time.Hour)
	if _, err := w.verify(w.bearer(w.coreErasureClaims())); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("dropped key: err = %v", err)
	}
}
