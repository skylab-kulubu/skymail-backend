package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

type idleMailer struct{}

func (idleMailer) Start(context.Context, int) {}

func (idleMailer) Enqueue(context.Context, database.CreateMailTaskParams) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (idleMailer) EnqueueSingle(context.Context, database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (idleMailer) EnqueueWithRecipients(context.Context, mailer.EnqueueWithRecipientsParams) (uuid.UUID, error) {
	return uuid.Nil, nil
}

type noGroups struct{}

func (noGroups) ListGroups(context.Context) ([]*gocloak.Group, error) { return nil, nil }

func (noGroups) GetGroup(context.Context, string) (*gocloak.Group, error) { return nil, nil }

func (noGroups) GetGroupMembers(context.Context, string) ([]*gocloak.User, error) { return nil, nil }

// mailRoutesApp serves the mail task routes as production registers them —
// the real permission middleware, handler, error handler and database — with
// only Keycloak's token check replaced by the roles under test.
func mailRoutesApp(t *testing.T, roles ...string) *fiber.App {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(context.Background(), postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", "31ef736f-72da-4a40-8791-d523199cf9f0")
		c.Locals("roles", roles)
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab")
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	return app
}

func TestSendSummaryAndStatusFilterRequireMailsRead(t *testing.T) {
	paths := []string{"/v1/mail_tasks/summary", "/v1/mail_tasks?status=failed"}

	for _, roles := range [][]string{
		{"skymail:access"},
		{"skymail:access", "skymail:mails:write", "skymail:mails:send", "skymail:templates:read", "skymail:lists:read"},
	} {
		app := mailRoutesApp(t, roles...)
		for _, path := range paths {
			response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != fiber.StatusForbidden || body.Code != "server.forbidden" || body.Message == "" {
				t.Errorf("roles %v: GET %s = %d %+v, want 403 server.forbidden", roles, path, response.StatusCode, body)
			}
		}
	}
}

func TestSendSummaryIsServedWithMailsRead(t *testing.T) {
	app := mailRoutesApp(t, "skymail:access", "skymail:mails:read")

	// /summary sits beside /:id; it has to reach the summary, not be read as a
	// task id.
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/mail_tasks/summary", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	var summary handlers.SendSummary
	if response.StatusCode != fiber.StatusOK || json.Unmarshal(body, &summary) != nil ||
		summary.TimeZone != "Europe/Istanbul" || len(summary.DailySent) != 30 {
		t.Fatalf("GET /v1/mail_tasks/summary = %d %s", response.StatusCode, body)
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/mail_tasks?status=failed", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusOK || string(body) != "[]" || response.Header.Get("X-Total-Count") != "0" {
		t.Fatalf("GET /v1/mail_tasks?status=failed = %d %s total=%q", response.StatusCode, body, response.Header.Get("X-Total-Count"))
	}

	// A bad value comes back in the API's error shape through the real handler.
	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/mail_tasks?status=bogus", nil))
	if err != nil {
		t.Fatal(err)
	}
	var apiError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusBadRequest || apiError.Code != "validation.error" || apiError.Message == "" {
		t.Fatalf("GET /v1/mail_tasks?status=bogus = %d %+v", response.StatusCode, apiError)
	}
}

func TestOpenAPIDocumentDescribesTheSendSummaryAndStatusFilter(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, nil)

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/docs/openapi.json", nil))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	queryParams := func(path string) map[string]bool {
		params := map[string]bool{}
		for _, p := range document.Paths[path]["get"].Parameters {
			if p.In == "query" {
				params[p.Name] = true
			}
		}
		return params
	}
	if summary := queryParams("/mail_tasks/summary"); !summary["days"] || !summary["recent"] {
		t.Errorf("GET /mail_tasks/summary query parameters = %v, want days and recent", summary)
	}
	if list := queryParams("/mail_tasks"); !list["status"] {
		t.Errorf("GET /mail_tasks query parameters = %v, want status", list)
	}
}
