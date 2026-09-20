package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
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
			api := protectedAPI(app, middlewares.NewAuthMiddleware("skymail", userinfo.URL), gate)
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
	api := protectedAPI(app, middlewares.NewAuthMiddleware("skymail", "http://userinfo.invalid"), gate)
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
	registerPublicRoutes(app, gate)

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
	registerPublicRoutes(app, nil)
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ready", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("response=%v err=%v", response, err)
	}
}
