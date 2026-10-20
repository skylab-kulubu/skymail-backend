package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// goneGroups is a Keycloak that has no group of any id: GetGroupMembers
// answers ErrGroupNotFound, as the real client does on Keycloak's 404.
type goneGroups struct{ noGroups }

func (goneGroups) GetGroupMembers(context.Context, string) ([]*gocloak.User, error) {
	return nil, keycloak.ErrGroupNotFound
}

// A mail_list_id that is neither an internal list nor a Keycloak group is
// unknown, never an empty audience: a send to it is 422
// mail.audience_unavailable and its recipients are 404, not a 500.
func TestAnUnknownKeycloakGroupIsRefusedNotEmpty(t *testing.T) {
	postgres := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	const templateID = "10000000-0000-4000-8000-000000000001"
	const groupID = "20000000-0000-4000-8000-000000000002"
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ($1, 'club.notice', 'Duyuru', 'K', '<p>x</p>', 'x', '')`, templateID); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", "31ef736f-72da-4a40-8791-d523199cf9f0")
		c.Locals("roles", []string{"skymail:access", "skymail:mails:write", "skymail:lists:read"})
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, goneGroups{}))
	api.Get("/mailing_lists/:id/recipients", auth.RequireAnyPermission("skymail:lists:read"), handlers.NewListHandler(store, goneGroups{}).GetRecipients)

	request := httptest.NewRequest(fiber.MethodPost, "/v1/mail_tasks",
		strings.NewReader(`{"template_id":"`+templateID+`","mail_list_id":"`+groupID+`"}`))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnprocessableEntity || body.Code != "mail.audience_unavailable" {
		t.Errorf("POST /v1/mail_tasks to an unknown group = %d %q, want 422 mail.audience_unavailable", response.StatusCode, body.Code)
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/mailing_lists/"+groupID+"/recipients", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNotFound {
		t.Errorf("GET recipients of an unknown group = %d, want 404", response.StatusCode)
	}
}
