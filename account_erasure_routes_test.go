package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/erasuretoken"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// PUT /internal/v1/account-erasures/{request_id}: core's Erasure command for
// SkyMail (ADR-0051; account erasure spec §2).

const (
	erasureIssuer   = "https://e.yildizskylab.com/realms/e-skylab"
	erasureSubject  = "3f1c9d70-5b8e-4c1a-9d2e-0a1b2c3d4e5f"
	erasureCaller   = "8d4f2c1e-7a6b-4c5d-9e8f-0a1b2c3d4e5f"
	erasureSchool   = "deniz.yilmaz@std.yildiz.edu.tr"
	erasurePersonal = "deniz@example.com"
)

// erasureLogs captures everything SkyMail logs during a test, so a test can
// say that no path wrote the person's subject or addresses.
type erasureLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *erasureLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *erasureLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *erasureLogs {
	t.Helper()
	logs := &erasureLogs{}
	previous := log.Logger
	log.Logger = zerolog.New(logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() {
		log.Logger = previous
		lower := strings.ToLower(logs.String())
		for _, secret := range []string{erasureSubject, erasureSchool, erasurePersonal, "deniz"} {
			if strings.Contains(lower, secret) {
				t.Errorf("logs carry %q:\n%s", secret, logs.String())
			}
		}
	})
	return logs
}

// erasureKeycloak signs tokens and serves its JWKS.
type erasureKeycloak struct {
	key     *rsa.PrivateKey
	down    atomic.Bool
	jwksURL string
}

func newErasureKeycloak(t *testing.T) *erasureKeycloak {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k := &erasureKeycloak{key: key}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if k.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kid": "rsa-1", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(server.Close)
	k.jwksURL = server.URL
	return k
}

func (k *erasureKeycloak) claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": erasureIssuer,
		"sub": erasureCaller,
		"azp": "core-erasure",
		"aud": []any{"skymail"},
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
		"resource_access": map[string]any{
			"skymail": map[string]any{"roles": []any{"skymail:account:erase"}},
		},
	}
}

func (k *erasureKeycloak) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "rsa-1"
	signed, err := token.SignedString(k.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// erasureGate answers the account access check per subject: allowed unless
// told otherwise.
type erasureGate struct {
	mu        sync.Mutex
	decisions map[string]accessgate.Decision
	checked   []string
}

func (g *erasureGate) Check(_ context.Context, subject string) accessgate.Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checked = append(g.checked, subject)
	if decision, ok := g.decisions[subject]; ok {
		return decision
	}
	return accessgate.Allowed
}

func (g *erasureGate) Ready(context.Context) error { return nil }

// erasureStoreStub stands in for Postgres where a test is about who may
// call the route: it records whether anything was erased.
type erasureStoreStub struct {
	receipt *database.AccountErasureReceipt
	erased  atomic.Int32
}

func (s *erasureStoreStub) FindAccountErasureReceipt(context.Context, uuid.UUID) (*database.AccountErasureReceipt, error) {
	return s.receipt, nil
}

func (s *erasureStoreStub) EraseAccount(_ context.Context, erasure database.AccountErasure) (*database.AccountErasureReceipt, error) {
	s.erased.Add(1)
	return &database.AccountErasureReceipt{RequestID: erasure.RequestID, CompletedAt: time.Now().UTC(), Counts: map[string]int64{"recipients_deleted": 0}}, nil
}

type erasureRoute struct {
	app      *fiber.App
	keycloak *erasureKeycloak
}

func newErasureRoute(t *testing.T, store handlers.AccountErasureStore, gate accessgate.Reader) *erasureRoute {
	t.Helper()
	keycloak := newErasureKeycloak(t)
	verifier := erasuretoken.NewVerifier(erasuretoken.Config{
		Issuer:         erasureIssuer,
		JWKSURL:        keycloak.jwksURL,
		ResourceClient: "skymail",
	})
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerInternalRoutes(app, handlers.NewAccountErasureHandler(store, verifier, gate))
	return &erasureRoute{app: app, keycloak: keycloak}
}

type erasureAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (a erasureAnswer) code() string {
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(a.body, &problem)
	return problem.Code
}

func (r *erasureRoute) put(t *testing.T, path, token, contentType string, body []byte, headers map[string]string) erasureAnswer {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodPut, path, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set(fiber.HeaderContentType, contentType)
	}
	if token != "" {
		request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := r.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return erasureAnswer{status: response.StatusCode, header: response.Header, body: raw}
}

func erasureCommandBody(requestID uuid.UUID, subject string, emails ...string) []byte {
	if emails == nil {
		emails = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"request_id": requestID.String(), "subject_id": subject, "emails": emails})
	return raw
}

// command sends a well-formed command with core-erasure's token.
func (r *erasureRoute) command(t *testing.T, requestID uuid.UUID) erasureAnswer {
	t.Helper()
	return r.put(t, "/internal/v1/account-erasures/"+requestID.String(), r.keycloak.token(t, r.keycloak.claims()),
		fiber.MIMEApplicationJSON, erasureCommandBody(requestID, erasureSubject, erasureSchool, erasurePersonal), nil)
}

func assertErasureProblem(t *testing.T, answer erasureAnswer, status int, code string) {
	t.Helper()
	if answer.status != status {
		t.Fatalf("status = %d %s, want %d", answer.status, answer.body, status)
	}
	if !strings.HasPrefix(answer.header.Get(fiber.HeaderContentType), "application/problem+json") {
		t.Errorf("content type = %q", answer.header.Get(fiber.HeaderContentType))
	}
	if got := answer.code(); got != code {
		t.Errorf("code = %q, want %q (%s)", got, code, answer.body)
	}
	lower := strings.ToLower(string(answer.body))
	for _, secret := range []string{erasureSubject, erasureSchool, erasurePersonal} {
		if strings.Contains(lower, secret) {
			t.Errorf("answer echoes %q: %s", secret, answer.body)
		}
	}
}

func TestErasureRouteRefusesWhoeverIsNotCoreErasure(t *testing.T) {
	captureLogs(t)
	store := &erasureStoreStub{}
	gate := &erasureGate{decisions: map[string]accessgate.Decision{erasureSubject: accessgate.Blocked}}
	route := newErasureRoute(t, store, gate)
	k := route.keycloak
	requestID := uuid.New()
	path := "/internal/v1/account-erasures/" + requestID.String()
	body := erasureCommandBody(requestID, erasureSubject, erasureSchool, erasurePersonal)

	t.Run("no token is 401", func(t *testing.T) {
		answer := route.put(t, path, "", fiber.MIMEApplicationJSON, body, nil)
		assertErasureProblem(t, answer, fiber.StatusUnauthorized, "erasure_unauthorized")
		if !strings.HasPrefix(answer.header.Get(fiber.HeaderWWWAuthenticate), "Bearer") {
			t.Errorf("WWW-Authenticate = %q", answer.header.Get(fiber.HeaderWWWAuthenticate))
		}
	})
	t.Run("an expired token is 401", func(t *testing.T) {
		claims := k.claims()
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		assertErasureProblem(t, route.put(t, path, k.token(t, claims), fiber.MIMEApplicationJSON, body, nil),
			fiber.StatusUnauthorized, "erasure_unauthorized")
	})
	for name, change := range map[string]func(jwt.MapClaims){
		"wrong azp":      func(c jwt.MapClaims) { c["azp"] = "core" },
		"wrong audience": func(c jwt.MapClaims) { c["aud"] = []any{"skycms"} },
		"no erase role": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"skymail": map[string]any{"roles": []any{"skymail:access", "skymail:mails:send"}}}
		},
		"erase role on another client": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"core-erasure": map[string]any{"roles": []any{"skymail:account:erase"}}}
		},
		"another service's erase role": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"skycms": map[string]any{"roles": []any{"cms:account:erase"}}}
		},
		"right role, another caller azp": func(c jwt.MapClaims) { c["azp"] = "forms" },
	} {
		t.Run(name+" is 403", func(t *testing.T) {
			claims := k.claims()
			change(claims)
			assertErasureProblem(t, route.put(t, path, k.token(t, claims), fiber.MIMEApplicationJSON, body, nil),
				fiber.StatusForbidden, "erasure_forbidden")
		})
	}
	for _, header := range [][2]string{
		{"X-Forwarded-For", "203.0.113.7"},
		{"X-Forwarded-Proto", "https"},
		{"X-Forwarded-Host", "skymail-api.yildizskylab.com"},
		{"X-Real-Ip", "203.0.113.7"},
		{"Forwarded", "for=203.0.113.7;proto=https"},
	} {
		t.Run("through the ingress ("+header[0]+") is 404", func(t *testing.T) {
			answer := route.put(t, path, k.token(t, k.claims()), fiber.MIMEApplicationJSON, body, map[string]string{header[0]: header[1]})
			if answer.status != fiber.StatusNotFound || len(answer.body) != 0 {
				t.Fatalf("answer = %d %q, want a bare 404", answer.status, answer.body)
			}
		})
	}
	t.Run("the /v1 API is not reached through this path", func(t *testing.T) {
		answer := route.put(t, "/internal/v1/mail_tasks", k.token(t, k.claims()), fiber.MIMEApplicationJSON, body, nil)
		if answer.status != fiber.StatusNotFound {
			t.Fatalf("status = %d", answer.status)
		}
	})

	if n := store.erased.Load(); n != 0 {
		t.Fatalf("erased %d times", n)
	}
	for _, subject := range gate.checked {
		if subject == erasureSubject {
			t.Fatal("the subject's marker was read for a refused caller")
		}
	}
}

func TestErasureRouteNeedsTheSubjectBlocked(t *testing.T) {
	captureLogs(t)
	requestID := uuid.New()

	t.Run("no marker is 409", func(t *testing.T) {
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, &erasureGate{})
		assertErasureProblem(t, route.command(t, requestID), fiber.StatusConflict, "subject_not_blocked")
		if store.erased.Load() != 0 {
			t.Fatal("erased an unblocked subject")
		}
	})
	t.Run("an unreadable marker is 503", func(t *testing.T) {
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, &erasureGate{decisions: map[string]accessgate.Decision{erasureSubject: accessgate.Unavailable}})
		answer := route.command(t, requestID)
		assertErasureProblem(t, answer, fiber.StatusServiceUnavailable, "subject_block_unverifiable")
		if answer.header.Get(fiber.HeaderRetryAfter) == "" || store.erased.Load() != 0 {
			t.Fatalf("Retry-After %q, erased %d", answer.header.Get(fiber.HeaderRetryAfter), store.erased.Load())
		}
	})
	t.Run("gate off, as in sandbox, cannot prove the block: 503, not 409", func(t *testing.T) {
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, nil)
		answer := route.command(t, requestID)
		assertErasureProblem(t, answer, fiber.StatusServiceUnavailable, "subject_block_unverifiable")
		if answer.header.Get(fiber.HeaderRetryAfter) != "300" || store.erased.Load() != 0 {
			t.Fatalf("Retry-After %q, erased %d", answer.header.Get(fiber.HeaderRetryAfter), store.erased.Load())
		}
	})
	t.Run("account access Redis down is 503", func(t *testing.T) {
		client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
		t.Cleanup(func() { _ = client.Close() })
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, accessgate.NewRedisGate(client, 100*time.Millisecond))
		answer := route.command(t, requestID)
		// The caller's own marker is read first, from the same Redis.
		assertErasureProblem(t, answer, fiber.StatusServiceUnavailable, "access_gate_unavailable")
		if store.erased.Load() != 0 {
			t.Fatal("erased with the gate down")
		}
	})
	t.Run("a blocked caller is 401", func(t *testing.T) {
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, &erasureGate{decisions: map[string]accessgate.Decision{
			erasureSubject: accessgate.Blocked, erasureCaller: accessgate.Blocked,
		}})
		assertErasureProblem(t, route.command(t, requestID), fiber.StatusUnauthorized, "erasure_unauthorized")
		if store.erased.Load() != 0 {
			t.Fatal("erased for a blocked caller")
		}
	})
	t.Run("Keycloak's keys unreachable is 503", func(t *testing.T) {
		store := &erasureStoreStub{}
		route := newErasureRoute(t, store, &erasureGate{decisions: map[string]accessgate.Decision{erasureSubject: accessgate.Blocked}})
		route.keycloak.down.Store(true)
		answer := route.command(t, requestID)
		assertErasureProblem(t, answer, fiber.StatusServiceUnavailable, "token_keys_unavailable")
		if answer.header.Get(fiber.HeaderRetryAfter) == "" || store.erased.Load() != 0 {
			t.Fatal("no Retry-After, or erased")
		}
	})
	t.Run("a finished request answers its receipt without reading the marker", func(t *testing.T) {
		completed := time.Date(2026, 9, 25, 12, 0, 0, 123456000, time.UTC)
		store := &erasureStoreStub{receipt: &database.AccountErasureReceipt{
			RequestID: requestID, CompletedAt: completed, Counts: map[string]int64{"recipients_deleted": 2},
		}}
		gate := &erasureGate{}
		route := newErasureRoute(t, store, gate)
		answer := route.command(t, requestID)
		want := `{"request_id":"` + requestID.String() + `","status":"completed","completed_at":"2026-09-25T12:00:00.123456Z","counts":{"recipients_deleted":2}}`
		if answer.status != fiber.StatusOK || string(answer.body) != want {
			t.Fatalf("answer = %d %s\nwant %s", answer.status, answer.body, want)
		}
		if store.erased.Load() != 0 || !reflect.DeepEqual(gate.checked, []string{erasureCaller}) {
			t.Fatalf("erased %d, checked %v: want only the caller's marker read", store.erased.Load(), gate.checked)
		}
	})
}

func TestErasureRouteRefusesAMalformedCommandWithoutEchoingIt(t *testing.T) {
	captureLogs(t)
	store := &erasureStoreStub{}
	route := newErasureRoute(t, store, &erasureGate{decisions: map[string]accessgate.Decision{erasureSubject: accessgate.Blocked}})
	token := route.keycloak.token(t, route.keycloak.claims())
	requestID := uuid.New()
	rid, other := requestID.String(), uuid.NewString()
	long := strings.Repeat("a", 250) + "@x.tr"

	for name, raw := range map[string]string{
		"request_id differs from the path": `{"request_id":"` + other + `","subject_id":"` + erasureSubject + `","emails":[]}`,
		"unknown field":                    `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[],"name":"Deniz"}`,
		"duplicate field":                  `{"request_id":"` + rid + `","request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[]}`,
		"missing request_id":               `{"subject_id":"` + erasureSubject + `","emails":[]}`,
		"missing subject_id":               `{"request_id":"` + rid + `","emails":[]}`,
		"missing emails":                   `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `"}`,
		"null subject_id":                  `{"request_id":"` + rid + `","subject_id":null,"emails":[]}`,
		"null emails":                      `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":null}`,
		"subject_id not a UUID":            `{"request_id":"` + rid + `","subject_id":"deniz","emails":[]}`,
		"subject_id not canonical":         `{"request_id":"` + rid + `","subject_id":"` + strings.ToUpper(erasureSubject) + `","emails":[]}`,
		"subject_id is Silinmiş kullanıcı": `{"request_id":"` + rid + `","subject_id":"00000000-0000-4000-8000-000000000000","emails":[]}`,
		"four emails":                      `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":["a@x.tr","b@x.tr","c@x.tr","d@x.tr"]}`,
		"empty email":                      `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[""]}`,
		"not an address":                   `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":["deniz"]}`,
		"address with a space":             `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":["deniz @example.com"]}`,
		"email over 254 characters":        `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":["` + long + `"]}`,
		"emails not an array":              `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":"` + erasurePersonal + `"}`,
		"email not a string":               `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[1]}`,
		"body not an object":               `["` + rid + `","` + erasureSubject + `"]`,
		"malformed JSON":                   `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `",`,
		"two objects":                      `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[]}{}`,
		"body over 4 KB":                   `{"request_id":"` + rid + `","subject_id":"` + erasureSubject + `","emails":[]` + strings.Repeat(" ", 4096) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			answer := route.put(t, "/internal/v1/account-erasures/"+rid, token, fiber.MIMEApplicationJSON, []byte(raw), nil)
			assertErasureProblem(t, answer, fiber.StatusBadRequest, "invalid_erasure_command")
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		answer := route.put(t, "/internal/v1/account-erasures/"+rid, token, fiber.MIMETextPlain,
			erasureCommandBody(requestID, erasureSubject, erasurePersonal), nil)
		assertErasureProblem(t, answer, fiber.StatusBadRequest, "invalid_erasure_command")
	})
	t.Run("request id in the path not a UUID", func(t *testing.T) {
		answer := route.put(t, "/internal/v1/account-erasures/"+erasurePersonal, token, fiber.MIMEApplicationJSON,
			erasureCommandBody(requestID, erasureSubject, erasurePersonal), nil)
		assertErasureProblem(t, answer, fiber.StatusBadRequest, "invalid_erasure_command")
	})
	t.Run("no addresses is a valid command", func(t *testing.T) {
		answer := route.put(t, "/internal/v1/account-erasures/"+rid, token, fiber.MIMEApplicationJSON,
			erasureCommandBody(requestID, erasureSubject), nil)
		if answer.status != fiber.StatusOK {
			t.Fatalf("status = %d %s", answer.status, answer.body)
		}
	})
	if n := store.erased.Load(); n != 1 {
		t.Fatalf("erased %d times, want only the valid command", n)
	}
}

// startAccessRedis runs a Redis whose skymail-reader user has the reader ACL
// of the account-access Redis — GET, MGET and connection commands on the
// namespace, nothing else — and returns a writer client standing in for
// core and the reader client SkyMail's gate uses.
func startAccessRedis(t *testing.T) (writer, reader *redis.Client) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("skymail-redis-test-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::6379", "redis:7-alpine",
		"redis-server", "--save", "", "--appendonly", "no",
		"--user", "default", "off",
		"--user", "core-writer", "on", ">writer-test", "~skylab:account-access:v1:*", "resetchannels", "-@all", "+@connection", "+get", "+mget", "+set",
		"--user", "skymail-reader", "on", ">reader-test", "~skylab:account-access:v1:*", "resetchannels", "-@all", "+@connection", "+get", "+mget",
	).CombinedOutput()
	if err != nil {
		t.Skipf("docker run redis: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })

	var hostport string
	deadline := time.Now().Add(30 * time.Second)
	for hostport == "" {
		out, err := exec.Command("docker", "port", name, "6379/tcp").CombinedOutput()
		if err == nil {
			hostport = strings.TrimSpace(strings.Split(string(out), "\n")[0])
		}
		if hostport == "" {
			if time.Now().After(deadline) {
				t.Fatalf("redis port never published: %s", out)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	writer = redis.NewClient(&redis.Options{Addr: hostport, Username: "core-writer", Password: "writer-test"})
	reader = redis.NewClient(&redis.Options{Addr: hostport, Username: "skymail-reader", Password: "reader-test", MaxRetries: -1})
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	for writer.Ping(context.Background()).Err() != nil {
		if time.Now().After(deadline) {
			t.Fatal("redis never became ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := writer.Set(context.Background(), accessgate.ContractKey, accessgate.ContractValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	return writer, reader
}

func TestErasureRouteErasesOnceAndRepeatsItsAnswer(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	writer, reader := startAccessRedis(t)
	route := newErasureRoute(t, store, accessgate.NewRedisGate(reader, time.Second))

	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('10000000-0000-4000-8000-000000000001', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', '');
		INSERT INTO mailing_lists (id, name) VALUES ('20000000-0000-4000-8000-000000000001', 'Tüm üyeler');
		INSERT INTO recipients (id, full_name, email) VALUES
		    ('30000000-0000-4000-8000-000000000001', 'Deniz Yılmaz', 'Deniz@Example.com'),
		    ('30000000-0000-4000-8000-000000000003', 'Ayşe Kaya', 'ayse@example.com');
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) VALUES
		    ('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000001'),
		    ('20000000-0000-4000-8000-000000000001', '30000000-0000-4000-8000-000000000003');
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables) VALUES
		    ('40000000-0000-4000-8000-00000000000a', '`+erasureSubject+`', '10000000-0000-4000-8000-000000000001',
		     '20000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status) VALUES
		    ('50000000-0000-4000-8000-0000000000a1', '40000000-0000-4000-8000-00000000000a', 'Deniz Yılmaz', 'deniz@example.com',
		     'Duyuru', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'processing'),
		    ('50000000-0000-4000-8000-0000000000a2', '40000000-0000-4000-8000-00000000000a', 'Ayşe Kaya', 'ayse@example.com',
		     'Duyuru', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'sent');`); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()

	// Core has not blocked the person yet: nothing happens.
	assertErasureProblem(t, route.command(t, requestID), fiber.StatusConflict, "subject_not_blocked")
	if err := writer.Set(ctx, accessgate.MarkerKey(erasureSubject), accessgate.MarkerValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	// The reader ACL cannot write: SkyMail only ever reads the marker.
	if err := reader.Set(ctx, accessgate.MarkerKey(erasureSubject), "0", 0).Err(); err == nil {
		t.Fatal("the reader could write a marker")
	}

	// A send to the person is going out: what can be done is done, and core
	// is asked to come back.
	answer := route.command(t, requestID)
	if answer.status != fiber.StatusAccepted || answer.header.Get(fiber.HeaderRetryAfter) != "30" ||
		string(answer.body) != `{"request_id":"`+requestID.String()+`","status":"in_progress"}` {
		t.Fatalf("in flight: %d %v %s", answer.status, answer.header, answer.body)
	}
	var sentBy string
	if err := postgres.Pool.QueryRow(ctx, `SELECT sent_by FROM mail_tasks`).Scan(&sentBy); err != nil || sentBy != database.DeletedUserSubject {
		t.Fatalf("sent_by after 202 = %q %v", sentBy, err)
	}

	if _, err := postgres.Pool.Exec(ctx, `UPDATE mail_queue SET status = 'sent' WHERE id = '50000000-0000-4000-8000-0000000000a1'`); err != nil {
		t.Fatal(err)
	}

	// Two commands at once: one does the work, both answer the same.
	answers := make([]erasureAnswer, 2)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = route.command(t, requestID)
		}()
	}
	wg.Wait()
	for _, a := range answers {
		if a.status != fiber.StatusOK || !bytes.Equal(a.body, answers[0].body) {
			t.Fatalf("answers = %d %s / %d %s", answers[0].status, answers[0].body, a.status, a.body)
		}
	}
	var body struct {
		RequestID   string           `json:"request_id"`
		Status      string           `json:"status"`
		CompletedAt time.Time        `json:"completed_at"`
		Counts      map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(answers[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != requestID.String() || body.Status != "completed" || body.CompletedAt.IsZero() ||
		body.Counts["recipients_deleted"] != 1 || body.Counts["queue_rows_cleared"] != 1 || len(body.Counts) > 32 {
		t.Fatalf("body = %+v", body)
	}
	for key, n := range body.Counts {
		if n < 0 || key != strings.ToLower(key) {
			t.Errorf("count %s = %d", key, n)
		}
	}

	// Repeating it answers the first 200 byte for byte, and does nothing.
	again := route.command(t, requestID)
	if again.status != fiber.StatusOK || !bytes.Equal(again.body, answers[0].body) {
		t.Fatalf("repeat = %d %s, want %s", again.status, again.body, answers[0].body)
	}
	var receipts int
	if err := postgres.Pool.QueryRow(ctx, `SELECT count(*) FROM account_erasure_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipts = %d %v", receipts, err)
	}
	var left int
	if err := postgres.Pool.QueryRow(ctx, `SELECT count(*) FROM recipients WHERE lower(email) = 'deniz@example.com'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("recipient left: %d %v", left, err)
	}

	if !strings.Contains(logs.String(), requestID.String()) {
		t.Errorf("logs do not name the request:\n%s", logs.String())
	}
}
