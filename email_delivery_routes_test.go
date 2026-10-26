package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// GET /v1/me/email-delivery (hub ticket rabbitmq-olaylar 10) through the
// chain production registers: a person reads which of their own addresses
// mail does not reach, with their own token, from the suppression list.

// The suppression list the tests read: two rows suppressed with Ayşe's sub,
// two of her addresses suppressed with no person (a sign-in code, a Forms
// mail), and two that are not hers.
const deliverySuppressions = `
INSERT INTO mail_suppressions (address, recipient_sub, source, reason_code, first_seen, last_seen)
VALUES ('ayse.old@example.com', '1a000000-0000-4000-8000-000000000001', 'dsn', '5.1.10', '2026-10-01T08:00:00Z', '2026-10-02T08:00:00Z'),
       ('ayse@example.com', NULL, 'smtp', 'RCPT TO 550 5.1.1', '2026-10-03T09:30:00Z', '2026-10-03T09:30:00Z'),
       ('ayse.kaya@std.yildiz.edu.tr', NULL, 'dsn', '5.4.1', '2026-10-05T10:00:00Z', '2026-10-05T10:00:00Z'),
       ('mehmet@example.com', '1a000000-0000-4000-8000-000000000002', 'dsn', '5.1.10', '2026-10-04T08:00:00Z', '2026-10-04T08:00:00Z'),
       ('stranger@example.com', NULL, 'dsn', '5.1.10', '2026-10-06T08:00:00Z', '2026-10-06T08:00:00Z');`

type deliveryWorld struct {
	t     *testing.T
	store *database.Store
	app   *fiber.App
}

// echoUserinfo answers userinfo with the bearer's sub and what its
// test_userinfo claim says userinfo carries (email, email_verified,
// school_email), as Keycloak would for a token it issued.
func echoUserinfo(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if r.URL.Path != "/protocol/openid-connect/userinfo" || len(parts) != 3 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Sub      string         `json:"sub"`
			Userinfo map[string]any `json:"test_userinfo"`
		}
		_ = json.Unmarshal(payload, &claims)
		body := map[string]any{"sub": claims.Sub}
		for k, v := range claims.Userinfo {
			body[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// newDeliveryWorld is the person's routes as main registers them, on a real
// database holding deliverySuppressions. Build it after redirecting the log.
func newDeliveryWorld(t *testing.T) *deliveryWorld {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, deliverySuppressions); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	auth := middlewares.NewAuthMiddleware("skymail", echoUserinfo(t), middlewares.AudienceOff)
	registerInboxRoutes(app, auth, nil, handlers.NewInboxHandler(store))
	registerEmailDeliveryRoutes(app, auth, nil, handlers.NewEmailDeliveryHandler(store))
	protectedAPI(app, auth, nil)
	return &deliveryWorld{t: t, store: store, app: app}
}

// deliveryToken is a person's token for SkyMail whose userinfo carries
// userinfo; claims go into the token itself.
func deliveryToken(t *testing.T, sub uuid.UUID, userinfo map[string]any, claims map[string]any) string {
	t.Helper()
	all := jwt.MapClaims{"sub": sub.String(), "azp": "account-center", "aud": []string{"skymail"}, "test_userinfo": userinfo}
	for k, v := range claims {
		all[k] = v
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, all).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type deliveryAnswer struct {
	status       int
	raw          string
	cacheControl string
	addresses    []string
	body         handlers.EmailDelivery
}

func (w *deliveryWorld) get(token, path string) deliveryAnswer {
	w.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	answer := deliveryAnswer{status: response.StatusCode, raw: string(raw), cacheControl: response.Header.Get(fiber.HeaderCacheControl)}
	if answer.status == fiber.StatusOK {
		if err := json.Unmarshal([]byte(answer.raw), &answer.body); err != nil {
			w.t.Fatalf("%s: %v %s", path, err, answer.raw)
		}
		for _, row := range answer.body.Undeliverable {
			answer.addresses = append(answer.addresses, row.Address)
		}
		sort.Strings(answer.addresses)
	}
	return answer
}

// The caller's own suppressions only: the rows with their sub, and the rows
// of the addresses their token says are theirs — whatever the case or spaces,
// each once — never another person's row or an address the token does not
// name, whatever the query string asks.
func TestEmailDeliveryServesOnlyTheCallersOwnSuppressions(t *testing.T) {
	sink := &lockedBuffer{}
	previous := log.Logger
	log.Logger = zerolog.New(sink)
	t.Cleanup(func() { log.Logger = previous })
	w := newDeliveryWorld(t)

	for _, c := range []struct {
		name     string
		userinfo map[string]any
		claims   map[string]any
		path     string
		want     []string
	}{
		{"sub only: no address in the token", nil, nil, "/v1/me/email-delivery", []string{"ayse.old@example.com"}},
		{"the token's email, suppressed with no person, in another case and with spaces",
			map[string]any{"email": "  Ayse@Example.COM ", "email_verified": true}, nil, "/v1/me/email-delivery",
			[]string{"ayse.old@example.com", "ayse@example.com"}},
		{"an email userinfo does not say is verified is not hers to ask about",
			map[string]any{"email": "ayse@example.com"}, nil, "/v1/me/email-delivery",
			[]string{"ayse.old@example.com"}},
		{"an email Keycloak has not verified is not hers to ask about",
			map[string]any{"email": "ayse@example.com", "email_verified": false}, nil, "/v1/me/email-delivery",
			[]string{"ayse.old@example.com"}},
		{"the school address userinfo carries",
			map[string]any{"email": "ayse@example.com", "email_verified": true, "school_email": "Ayse.Kaya@std.yildiz.edu.tr"}, nil, "/v1/me/email-delivery",
			[]string{"ayse.kaya@std.yildiz.edu.tr", "ayse.old@example.com", "ayse@example.com"}},
		{"the school address only the token carries",
			map[string]any{"email": "ayse@example.com", "email_verified": true}, map[string]any{"school_email": "ayse.kaya@std.yildiz.edu.tr"}, "/v1/me/email-delivery",
			[]string{"ayse.kaya@std.yildiz.edu.tr", "ayse.old@example.com", "ayse@example.com"}},
		{"matched by sub and by address: once",
			map[string]any{"email": "ayse.old@example.com", "email_verified": true}, nil, "/v1/me/email-delivery",
			[]string{"ayse.old@example.com"}},
		{"a query string asks for others: ignored",
			map[string]any{"email": "ayse@example.com", "email_verified": true}, nil,
			"/v1/me/email-delivery?address=stranger@example.com&email=mehmet@example.com&sub=1a000000-0000-4000-8000-000000000002&addresses=stranger@example.com",
			[]string{"ayse.old@example.com", "ayse@example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := w.get(deliveryToken(t, personAyse, c.userinfo, c.claims), c.path)
			if got.status != fiber.StatusOK || !reflect.DeepEqual(got.addresses, c.want) {
				t.Fatalf("%d %s, want %v", got.status, got.raw, c.want)
			}
			if got.cacheControl != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got.cacheControl)
			}
		})
	}

	// Mehmet sees his own row, never Ayşe's.
	if got := w.get(deliveryToken(t, personMehmet, map[string]any{"email": "mehmet@example.com", "email_verified": true}, nil), "/v1/me/email-delivery"); got.status != fiber.StatusOK ||
		!reflect.DeepEqual(got.addresses, []string{"mehmet@example.com"}) {
		t.Fatalf("mehmet: %d %s", got.status, got.raw)
	}

	// Each row is the address and since (first_seen, UTC), nothing else: no
	// source, code or id.
	got := w.get(deliveryToken(t, personAyse, map[string]any{"email": "ayse@example.com", "email_verified": true}, nil), "/v1/me/email-delivery")
	var shape map[string][]map[string]any
	if err := json.Unmarshal([]byte(got.raw), &shape); err != nil || len(shape) != 1 || len(shape["undeliverable"]) != 2 {
		t.Fatalf("body %s", got.raw)
	}
	for _, row := range shape["undeliverable"] {
		keys := []string{}
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"address", "since"}) {
			t.Errorf("row keys %v, want [address since]", keys)
		}
	}
	for _, leak := range []string{"dsn", "smtp", "5.1", "RCPT", "source", "reason", `"id"`} {
		if strings.Contains(got.raw, leak) {
			t.Errorf("body carries %q: %s", leak, got.raw)
		}
	}
	since := map[string]string{}
	for _, row := range shape["undeliverable"] {
		since[row["address"].(string)] = row["since"].(string)
	}
	if since["ayse.old@example.com"] != "2026-10-01T08:00:00Z" || since["ayse@example.com"] != "2026-10-03T09:30:00Z" {
		t.Errorf("since %v", since)
	}

	// No suppression: an empty list, not null.
	if got := w.get(deliveryToken(t, personZeynep, map[string]any{"email": "zeynep@example.com", "email_verified": true}, nil), "/v1/me/email-delivery"); got.status != fiber.StatusOK ||
		got.raw != `{"undeliverable":[]}` {
		t.Fatalf("zeynep: %d %s", got.status, got.raw)
	}

	// The log has the request's id and the count, never an address or a sub.
	logs := sink.String()
	if !strings.Contains(logs, `"event":"email_delivery_read"`) || !strings.Contains(logs, `"undeliverable":2`) || !strings.Contains(logs, `"correlation_id"`) {
		t.Errorf("no read line with the count: %s", logs)
	}
	for _, secret := range []string{"@", personAyse.String(), personMehmet.String(), personZeynep.String(), "ayse", "mehmet"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log carries %q: %s", secret, logs)
		}
	}
}

// Only a person's own token issued for SkyMail reads it, as the inbox.
func TestOnlyAPersonsOwnTokenForSkyMailReadsEmailDelivery(t *testing.T) {
	w := newDeliveryWorld(t)
	for _, c := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"a person's token for SkyMail, no role", personToken(t, personAyse, "skymail"), fiber.StatusOK, ""},
		{"a person's token for another service", personToken(t, personAyse, "account"), fiber.StatusUnauthorized, "server.unauthorized"},
		{"a person's token with no aud", personToken(t, personAyse), fiber.StatusUnauthorized, "server.unauthorized"},
		{"a service's token", serviceToken(t, "skymail"), fiber.StatusForbidden, "auth.person_required"},
		{"no token", "", fiber.StatusUnauthorized, "server.unauthorized"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := w.get(c.token, "/v1/me/email-delivery")
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal([]byte(got.raw), &body)
			if got.status != c.status || (c.code != "" && body.Code != c.code) {
				t.Errorf("%d %s, want %d %s", got.status, got.raw, c.status, c.code)
			}
			if c.status != fiber.StatusOK && strings.Contains(got.raw, "@") {
				t.Errorf("a refusal carries an address: %s", got.raw)
			}
		})
	}
}

// recordingDelivery is an EmailDeliveryHandler that only says it ran.
type recordingDelivery struct{ order *[]string }

func (r recordingDelivery) Get(c fiber.Ctx) error {
	*r.order = append(*r.order, "handler")
	return c.SendStatus(fiber.StatusNoContent)
}

// The route runs the person check, then the account access gate, then its
// handler — the inbox's answer when the gate is closed — and never /v1's
// Authenticate or permission check, though protectedAPI is registered after
// it as in main.
func TestEmailDeliveryRunsThePersonCheckAndTheGateOnly(t *testing.T) {
	t.Parallel()
	for _, decision := range []accessgate.Decision{accessgate.Allowed, accessgate.Blocked, accessgate.Unavailable} {
		order := []string{}
		auth := &routeTestAuth{subject: personAyse.String(), order: &order}
		gate := &routeTestGate{decision: decision, order: &order}
		app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
		registerInboxRoutes(app, auth, gate, recordingInbox{order: &order})
		registerEmailDeliveryRoutes(app, auth, gate, recordingDelivery{order: &order})
		api := protectedAPI(app, auth, gate)
		api.Get("/me/email-delivery", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusTeapot) })

		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/me/email-delivery", nil))
		if err != nil {
			t.Fatal(err)
		}
		want, wantOrder := fiber.StatusNoContent, []string{"person", "gate", "handler"}
		switch decision {
		case accessgate.Blocked:
			want, wantOrder = fiber.StatusUnauthorized, []string{"person", "gate"}
		case accessgate.Unavailable:
			want, wantOrder = fiber.StatusServiceUnavailable, []string{"person", "gate"}
		}
		if response.StatusCode != want || !reflect.DeepEqual(order, wantOrder) || auth.permissionCalls != 0 {
			t.Errorf("%s: status %d order %v permission %d, want %d %v", decision, response.StatusCode, order, auth.permissionCalls, want, wantOrder)
		}
	}
}
