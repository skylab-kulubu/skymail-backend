package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/health"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
)

func TestErrorHandlerMapsUniqueConflict(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	app.Post("/restore", func(fiber.Ctx) error {
		return &pgconn.PgError{Code: "23505", Message: "unique violation"}
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/restore", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
}

func TestServerRequestIDIgnoresUntrustedCorrelationID(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(serverRequestID())
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	request := httptest.NewRequest(fiber.MethodGet, "/", nil)
	request.Header.Set(fiber.HeaderXRequestID, "3babe8e3-4d2b-4d10-ba5f-95bfaf35c0dc")

	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	got := response.Header.Get(fiber.HeaderXRequestID)
	if got == "" || got == request.Header.Get(fiber.HeaderXRequestID) {
		t.Fatalf("server request ID = %q", got)
	}
}

type routeTestAuth struct {
	subject         string
	authCalls       int
	permissionCalls int
	order           *[]string
}

func (a *routeTestAuth) Authenticate(c fiber.Ctx) error {
	a.authCalls++
	*a.order = append(*a.order, "authenticate")
	c.Locals("user_id", a.subject)
	return c.Next()
}

func (a *routeTestAuth) RequireAnyPermission(...string) func(fiber.Ctx) error {
	return func(c fiber.Ctx) error {
		a.permissionCalls++
		*a.order = append(*a.order, "permission")
		return c.Next()
	}
}

type routeTestGate struct {
	decision accessgate.Decision
	readyErr error
	checks   int
	ready    int
	subjects []string
	order    *[]string
}

func (g *routeTestGate) Check(_ context.Context, subject string) accessgate.Decision {
	g.checks++
	g.subjects = append(g.subjects, subject)
	if g.order != nil {
		*g.order = append(*g.order, "gate")
	}
	return g.decision
}

func (g *routeTestGate) Ready(context.Context) error {
	g.ready++
	return g.readyErr
}

func TestProtectedAPIRunsGateBeforePermissionsForEveryRouteCategory(t *testing.T) {
	t.Parallel()

	for _, route := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "templates", method: fiber.MethodGet, path: "/v1/templates"},
		{name: "mailing lists", method: fiber.MethodPost, path: "/v1/mailing_lists"},
		{name: "mail tasks", method: fiber.MethodPost, path: "/v1/mail_tasks/single"},
	} {
		route := route
		t.Run(route.name, func(t *testing.T) {
			for _, decision := range []accessgate.Decision{accessgate.Blocked, accessgate.Unavailable} {
				t.Run(string(decision), func(t *testing.T) {
					order := []string{}
					auth := &routeTestAuth{subject: "authenticated-subject", order: &order}
					gate := &routeTestGate{decision: decision, order: &order}
					handlerCalls := 0
					app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
					api := protectedAPI(app, auth, gate)
					api.Add([]string{route.method}, strings.TrimPrefix(route.path, "/v1"), func(c fiber.Ctx) error {
						handlerCalls++
						order = append(order, "handler")
						return c.SendStatus(fiber.StatusNoContent)
					})

					response, err := app.Test(httptest.NewRequest(route.method, route.path, nil))
					if err != nil {
						t.Fatal(err)
					}
					wantStatus := fiber.StatusUnauthorized
					if decision == accessgate.Unavailable {
						wantStatus = fiber.StatusServiceUnavailable
					}
					if response.StatusCode != wantStatus || auth.permissionCalls != 0 || handlerCalls != 0 {
						t.Fatalf("status=%d permission=%d handler=%d", response.StatusCode, auth.permissionCalls, handlerCalls)
					}
					if !reflect.DeepEqual(order, []string{"authenticate", "gate"}) {
						t.Fatalf("middleware order = %v", order)
					}
				})
			}
		})
	}
}

func TestProtectedAPIAllowsOnlyAfterAuthenticateGateAndPermission(t *testing.T) {
	t.Parallel()

	order := []string{}
	auth := &routeTestAuth{subject: "service-account-skymail-backend", order: &order}
	gate := &routeTestGate{decision: accessgate.Allowed, order: &order}
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := protectedAPI(app, auth, gate)
	api.Get("/templates", func(c fiber.Ctx) error {
		order = append(order, "handler")
		return c.SendStatus(fiber.StatusNoContent)
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/templates", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if !reflect.DeepEqual(order, []string{"authenticate", "gate", "permission", "handler"}) {
		t.Fatalf("middleware order = %v", order)
	}
}

func TestProtectedAPIUsesUserInfoSubjectForHumanAndClientCredentials(t *testing.T) {
	t.Parallel()

	for _, subject := range []string{
		"3babe8e3-4d2b-4d10-ba5f-95bfaf35c0dc",
		"service-account-skymail-backend",
	} {
		subject := subject
		t.Run(subject, func(t *testing.T) {
			userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/protocol/openid-connect/userinfo" || r.Header.Get("Authorization") != "Bearer valid-token" {
					http.Error(w, "invalid request", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"sub": subject,
					"resource_access": map[string]any{
						"skymail": map[string]any{"roles": []string{"skymail:access"}},
					},
				})
			}))
			defer userinfo.Close()

			order := []string{}
			gate := &routeTestGate{decision: accessgate.Allowed, order: &order}
			app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			api := protectedAPI(app, middlewares.NewAuthMiddleware("skymail", userinfo.URL, middlewares.AudienceOff), gate)
			api.Get("/probe", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
			request := httptest.NewRequest(fiber.MethodGet, "/v1/probe", nil)
			request.Header.Set(fiber.HeaderAuthorization, "Bearer valid-token")

			response, err := app.Test(request)
			if err != nil || response.StatusCode != fiber.StatusNoContent {
				t.Fatalf("response=%v err=%v", response, err)
			}
			if !reflect.DeepEqual(gate.subjects, []string{subject}) {
				t.Fatalf("gate subjects = %v", gate.subjects)
			}
		})
	}
}

func TestProtectedAPIDoesNotQueryGateBeforeAuthenticationSucceeds(t *testing.T) {
	t.Parallel()

	gate := &routeTestGate{decision: accessgate.Allowed}
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := protectedAPI(app, middlewares.NewAuthMiddleware("skymail", "http://userinfo.invalid", middlewares.AudienceOff), gate)
	api.Get("/probe", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/probe", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusForbidden || gate.checks != 0 {
		t.Fatalf("status=%d gate checks=%d", response.StatusCode, gate.checks)
	}
}

func TestPublicDocsLivenessAndReadinessBehavior(t *testing.T) {
	t.Parallel()

	gate := &routeTestGate{decision: accessgate.Unavailable, readyErr: errors.New("contract mismatch")}
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, gate, nil)

	for _, path := range []string{"/health", "/docs/openapi.json"} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("GET %s status = %d", path, response.StatusCode)
		}
	}
	if gate.checks != 0 || gate.ready != 0 {
		t.Fatalf("public docs/liveness touched gate: checks=%d ready=%d", gate.checks, gate.ready)
	}

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusServiceUnavailable || gate.ready != 1 {
		t.Fatalf("readiness status=%d calls=%d body=%s", response.StatusCode, gate.ready, body)
	}
	if response.Header.Get(fiber.HeaderCacheControl) != "no-store" || response.Header.Get(fiber.HeaderRetryAfter) != "1" {
		t.Fatalf("readiness headers = %v", response.Header)
	}
	if strings.Contains(string(body), "contract") {
		t.Fatalf("readiness response leaked internal cause: %s", body)
	}
}

func TestReadinessIsProcessOnlyWhenGateIsOff(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, nil, nil)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("response=%v err=%v", response, err)
	}
}

type readyTestDatabase struct{ err error }

func (d readyTestDatabase) Ping(context.Context) error { return d.err }

func readyStatus(t *testing.T, app *fiber.App, path string) int {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// /ready is the task and its database, then the account access gate (enforce
// mode only); ?gate=skip, the container's health check, leaves the gate out.
// /health stays 204 whatever (release checks read it), and nothing says why.
func TestReadinessAsksTheDatabaseAndTheGateUnlessSkipped(t *testing.T) {
	t.Parallel()

	down := health.NewReadiness(readyTestDatabase{err: errors.New("connection refused")}, health.Options{})
	gate := &routeTestGate{readyErr: nil}
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, gate, down)
	for _, path := range []string{"/ready", "/ready?gate=skip"} {
		if got := readyStatus(t, app, path); got != fiber.StatusServiceUnavailable {
			t.Fatalf("database down: GET %s = %d, want 503", path, got)
		}
	}
	if gate.ready != 0 {
		t.Fatalf("the gate was asked %d times with the database down", gate.ready)
	}
	if got := readyStatus(t, app, "/health"); got != fiber.StatusNoContent {
		t.Fatalf("database down: GET /health = %d, want 204", got)
	}

	up := health.NewReadiness(readyTestDatabase{}, health.Options{})
	gateDown := &routeTestGate{readyErr: errors.New("contract mismatch")}
	app = fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, gateDown, up)
	if got := readyStatus(t, app, "/ready"); got != fiber.StatusServiceUnavailable || gateDown.ready != 1 {
		t.Fatalf("gate down: GET /ready = %d (gate asked %d), want 503 after asking it", got, gateDown.ready)
	}
	if got := readyStatus(t, app, "/ready?gate=skip"); got != fiber.StatusNoContent || gateDown.ready != 1 {
		t.Fatalf("gate down: GET /ready?gate=skip = %d (gate asked %d), want 204 without asking it", got, gateDown.ready)
	}

	up.Drain()
	for _, path := range []string{"/ready", "/ready?gate=skip"} {
		if got := readyStatus(t, app, path); got != fiber.StatusServiceUnavailable {
			t.Fatalf("draining: GET %s = %d, want 503", path, got)
		}
	}
	if got := readyStatus(t, app, "/health"); got != fiber.StatusNoContent {
		t.Fatalf("draining: GET /health = %d, want 204", got)
	}
}

func TestTrustedProxyRanges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{
			name: "unset falls back to the container network space",
			want: strings.Split(defaultTrustedProxyRanges, ","),
		},
		{
			name: "an explicit list is taken verbatim",
			raw:  " 10.0.1.0/24 ,, ::1/128 ",
			want: []string{"10.0.1.0/24", "::1/128"},
		},
		{
			name: "a bare address is a single host",
			raw:  "10.0.1.109",
			want: []string{"10.0.1.109"},
		},
		{
			name:    "a typo stops startup instead of being skipped",
			raw:     "10.0.0.0/8,10.0.0/8",
			wantErr: true,
		},
		{
			name:    "a hostname is not a range",
			raw:     "traefik",
			wantErr: true,
		},
		{
			name:    "a list with nothing in it trusts nobody by accident",
			raw:     " , ",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := trustedProxyRanges(func(key string) string {
				if key != "TRUSTED_PROXY_RANGES" {
					t.Fatalf("read %q", key)
				}
				return tc.raw
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("trustedProxyRanges(%q) = %v, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("trustedProxyRanges(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// An admin's bearer (many groups and roles) plus the browser's
// .yildizskylab.com cookies ran past Fiber's 4 KiB read buffer, and every
// call answered 431 wrapped as a 500 (2026-10-04).
func TestServerAcceptsLargeAuthorizationHeader(t *testing.T) {
	if got := getWithAuthorization(t, strings.Repeat("a", 10<<10)); got != fiber.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
}

func TestServerAnswersOversizedHeaderWith431(t *testing.T) {
	if got := getWithAuthorization(t, strings.Repeat("a", 20<<10)); got != fiber.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", got)
	}
}

// getWithAuthorization serves the production Fiber config on a real socket:
// app.Test reports a header that overflows the read buffer as its own error
// instead of returning the response the client gets.
func getWithAuthorization(t *testing.T, token string) int {
	t.Helper()
	app := fiber.New(newFiberConfig(nil, nil))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	request, err := http.NewRequest(fiber.MethodGet, "http://"+listener.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestErrorHandlerKeepsFiberClientErrorStatus(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	app.Post("/", func(fiber.Ctx) error { return fiber.ErrRequestEntityTooLarge })

	response, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
	var body struct {
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Params["original_code"] != float64(fiber.StatusRequestEntityTooLarge) {
		t.Fatalf("params = %v", body.Params)
	}
}
