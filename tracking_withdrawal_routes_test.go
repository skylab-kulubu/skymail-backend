package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/config"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/erasuretoken"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// PUT /internal/v1/tracking-withdrawals/{sub}: core telling SkyMail a person
// turned open and click tracking off on my. (hub ticket iletisim-servisi
// 14c; design note 07 §4.6).

const withdrawalSubject = "7a1e4b20-3c5d-4e6f-8a9b-0c1d2e3f4a5b"

type withdrawalStoreStub struct {
	mu       sync.Mutex
	subjects []string
	err      error
}

func (s *withdrawalStoreStub) WithdrawTracking(_ context.Context, subject string) (database.TrackingWithdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return database.TrackingWithdrawal{}, s.err
	}
	s.subjects = append(s.subjects, subject)
	return database.TrackingWithdrawal{TokensClosed: 2, EventsDeleted: 5}, nil
}

func (s *withdrawalStoreStub) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.subjects...)
}

func newWithdrawalRoute(t *testing.T, store handlers.TrackingWithdrawalStore, gate accessgate.Reader) *erasureRoute {
	t.Helper()
	keycloak := newErasureKeycloak(t)
	tokens := erasuretoken.Config{Issuer: erasureIssuer, JWKSURL: keycloak.jwksURL, ResourceClient: "skymail"}
	withdraw := tokens
	withdraw.CallerClientID = erasuretoken.WithdrawCallerClientID
	withdraw.Role = erasuretoken.WithdrawRole
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerInternalRoutes(app,
		handlers.NewAccountErasureHandler(&erasureStoreStub{}, erasuretoken.NewVerifier(tokens), gate),
		handlers.NewTrackingWithdrawalHandler(store, erasuretoken.NewVerifier(withdraw), gate))
	return &erasureRoute{app: app, keycloak: keycloak}
}

// coreClaims is core's own client credentials token with SkyMail's withdraw
// role.
func coreClaims(k *erasureKeycloak) jwt.MapClaims {
	claims := k.claims()
	claims["azp"] = "core"
	claims["resource_access"] = map[string]any{"skymail": map[string]any{"roles": []any{"skymail:tracking:withdraw"}}}
	return claims
}

func assertNoSubject(t *testing.T, logs *erasureLogs, answer erasureAnswer) {
	t.Helper()
	if strings.Contains(logs.String(), withdrawalSubject) || strings.Contains(string(answer.body), withdrawalSubject) {
		t.Errorf("the subject is logged or echoed:\n%s\n%s", logs.String(), answer.body)
	}
}

// core's token with the withdraw role withdraws, 204 with no body, each time
// the same; the log line has counts only.
func TestWithdrawalRouteWithdrawsForCore(t *testing.T) {
	logs := captureLogs(t)
	store := &withdrawalStoreStub{}
	gate := &erasureGate{}
	route := newWithdrawalRoute(t, store, gate)
	path := "/internal/v1/tracking-withdrawals/" + withdrawalSubject
	for i := 0; i < 2; i++ {
		answer := route.put(t, path, route.keycloak.token(t, coreClaims(route.keycloak)), "", nil, nil)
		if answer.status != fiber.StatusNoContent || len(answer.body) != 0 {
			t.Fatalf("answer %d = %d %q, want 204 and no body", i, answer.status, answer.body)
		}
		if got := answer.header.Get(fiber.HeaderCacheControl); got != "no-store" {
			t.Errorf("Cache-Control = %q", got)
		}
		assertNoSubject(t, logs, answer)
	}
	if got := store.calls(); len(got) != 2 || got[0] != withdrawalSubject {
		t.Errorf("withdrawn = %v", got)
	}
	if !strings.Contains(logs.String(), `"event":"tracking_withdrawal"`) || !strings.Contains(logs.String(), `"events_deleted":5`) {
		t.Errorf("no counts line:\n%s", logs.String())
	}
	// The caller's own access marker is checked, as on every route.
	if len(gate.checked) != 2 || gate.checked[0] != erasureCaller {
		t.Errorf("gate checked %v, want the caller", gate.checked)
	}
}

// Anyone but core with the withdraw role is refused before anything is read
// or changed: core-erasure's erase token is not it (ticket 17). The public
// ingress gets a bare 404. A subject that is not a lower-case UUID is 400.
func TestWithdrawalRouteRefusesWhoeverIsNotCore(t *testing.T) {
	logs := captureLogs(t)
	store := &withdrawalStoreStub{}
	route := newWithdrawalRoute(t, store, &erasureGate{decisions: map[string]accessgate.Decision{}})
	k := route.keycloak
	path := "/internal/v1/tracking-withdrawals/" + withdrawalSubject

	t.Run("no token is 401", func(t *testing.T) {
		answer := route.put(t, path, "", "", nil, nil)
		assertErasureProblem(t, answer, fiber.StatusUnauthorized, "withdrawal_unauthorized")
		if !strings.HasPrefix(answer.header.Get(fiber.HeaderWWWAuthenticate), "Bearer") {
			t.Errorf("WWW-Authenticate = %q", answer.header.Get(fiber.HeaderWWWAuthenticate))
		}
	})
	for name, change := range map[string]func(jwt.MapClaims){
		"core-erasure's erase token": func(c jwt.MapClaims) {
			c["azp"] = "core-erasure"
			c["resource_access"] = map[string]any{"skymail": map[string]any{"roles": []any{"skymail:account:erase"}}}
		},
		"the withdraw role on core-erasure": func(c jwt.MapClaims) { c["azp"] = "core-erasure" },
		"core without the role": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"skymail": map[string]any{"roles": []any{"skymail:mails:send"}}}
		},
		"the role on core's own client": func(c jwt.MapClaims) {
			c["resource_access"] = map[string]any{"core": map[string]any{"roles": []any{"skymail:tracking:withdraw"}}}
		},
		"not for SkyMail": func(c jwt.MapClaims) { c["aud"] = []any{"core"} },
	} {
		t.Run(name+" is 403", func(t *testing.T) {
			claims := coreClaims(k)
			change(claims)
			assertErasureProblem(t, route.put(t, path, k.token(t, claims), "", nil, nil), fiber.StatusForbidden, "withdrawal_forbidden")
		})
	}
	t.Run("through the ingress is 404", func(t *testing.T) {
		answer := route.put(t, path, k.token(t, coreClaims(k)), "", nil, map[string]string{"X-Forwarded-For": "203.0.113.7"})
		if answer.status != fiber.StatusNotFound || len(answer.body) != 0 {
			t.Fatalf("answer = %d %q, want a bare 404", answer.status, answer.body)
		}
	})
	for _, subject := range []string{
		strings.ToUpper(withdrawalSubject),
		"{" + withdrawalSubject + "}",
		strings.ReplaceAll(withdrawalSubject, "-", ""),
		"urn:uuid:" + withdrawalSubject,
		"not-a-subject",
		database.DeletedUserSubject,
	} {
		t.Run("subject "+subject+" is 400", func(t *testing.T) {
			assertErasureProblem(t, route.put(t, "/internal/v1/tracking-withdrawals/"+subject, k.token(t, coreClaims(k)), "", nil, nil),
				fiber.StatusBadRequest, "invalid_withdrawal_subject")
		})
	}
	if got := store.calls(); len(got) != 0 {
		t.Errorf("withdrawn for refused calls: %v", got)
	}
	assertNoSubject(t, logs, erasureAnswer{})
}

// What SkyMail cannot do now is 503 with Retry-After, for core's retrying
// job: Keycloak's keys, the access gate, the database.
func TestWithdrawalRouteUnavailableIsRetried(t *testing.T) {
	captureLogs(t)
	store := &withdrawalStoreStub{err: errors.New("database down")}
	route := newWithdrawalRoute(t, store, &erasureGate{})
	path := "/internal/v1/tracking-withdrawals/" + withdrawalSubject
	answer := route.put(t, path, route.keycloak.token(t, coreClaims(route.keycloak)), "", nil, nil)
	assertErasureProblem(t, answer, fiber.StatusServiceUnavailable, "withdrawal_store_unavailable")
	if answer.header.Get(fiber.HeaderRetryAfter) != "30" {
		t.Errorf("Retry-After = %q", answer.header.Get(fiber.HeaderRetryAfter))
	}

	gated := newWithdrawalRoute(t, &withdrawalStoreStub{}, &erasureGate{decisions: map[string]accessgate.Decision{erasureCaller: accessgate.Unavailable}})
	assertErasureProblem(t, gated.put(t, path, gated.keycloak.token(t, coreClaims(gated.keycloak)), "", nil, nil),
		fiber.StatusServiceUnavailable, "access_gate_unavailable")
	blocked := newWithdrawalRoute(t, &withdrawalStoreStub{}, &erasureGate{decisions: map[string]accessgate.Decision{erasureCaller: accessgate.Blocked}})
	assertErasureProblem(t, blocked.put(t, path, blocked.keycloak.token(t, coreClaims(blocked.keycloak)), "", nil, nil),
		fiber.StatusUnauthorized, "withdrawal_unauthorized")

	keys := newWithdrawalRoute(t, &withdrawalStoreStub{}, &erasureGate{})
	token := keys.keycloak.token(t, coreClaims(keys.keycloak))
	keys.keycloak.down.Store(true)
	assertErasureProblem(t, keys.put(t, path, token, "", nil, nil), fiber.StatusServiceUnavailable, "token_keys_unavailable")
}

// The running service checks the withdrawal token as the erase route checks
// its own — the realm's issuer and keys, SkyMail's client — but for core's
// client and the withdraw role.
func TestWithdrawalTokenConfig(t *testing.T) {
	cfg := config.Config{KeycloakRealmURL: erasureIssuer, KeycloakClientID: "skymail"}
	got, erase := withdrawalTokenConfig(cfg), erasureTokenConfig(cfg)
	if got.CallerClientID != "core" || got.Role != "skymail:tracking:withdraw" {
		t.Errorf("withdrawal caller %q role %q", got.CallerClientID, got.Role)
	}
	if got.Issuer != erase.Issuer || got.JWKSURL != erase.JWKSURL || got.ResourceClient != erase.ResourceClient || got.Issuer == "" {
		t.Errorf("withdrawal %+v, erase %+v", got, erase)
	}
	if erase.CallerClientID != "" || erase.Role != "" {
		t.Errorf("the erase route's config changed: %+v", erase)
	}
}
