package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// templateRoutesApp serves the template routes as production registers them —
// the real permission middleware, handler, error handler and database — with
// only Keycloak's token check replaced by the roles under test. It returns a
// template with one version to ask about.
func templateRoutesApp(t *testing.T, roles ...string) (*fiber.App, database.Template) {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(context.Background(), postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	template, err := store.PublishTemplateWrite(context.Background(), database.VersionAuthor{Kind: database.TemplateAuthorKindOperator}, nil,
		func(q *database.Queries) (database.Template, error) {
			return q.CreateTemplate(context.Background(), database.CreateTemplateParams{
				Name: "Bülten", Subject: "SKY LAB", HtmlContent: "<p>SKY LAB</p>", PlainTextContent: "SKY LAB", ReactEmailContent: "",
			})
		})
	if err != nil {
		t.Fatal(err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", "31ef736f-72da-4a40-8791-d523199cf9f0")
		c.Locals("roles", roles)
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab")
	registerTemplateRoutes(api, auth, handlers.NewTemplateHandler(store))
	return app, template
}

// Versions are read with the role every other template read needs.
func TestTemplateVersionsRequireTemplatesRead(t *testing.T) {
	for _, tc := range []struct {
		roles []string
		want  int
	}{
		{[]string{"skymail:access"}, fiber.StatusForbidden},
		{[]string{"skymail:access", "skymail:templates:write", "skymail:mails:read", "skymail:mails:send", "skymail:lists:read"}, fiber.StatusForbidden},
		{[]string{"skymail:access", "skymail:templates:read"}, fiber.StatusOK},
	} {
		app, template := templateRoutesApp(t, tc.roles...)

		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/templates/"+template.ID.String()+"/versions", nil))
		if err != nil {
			t.Fatal(err)
		}
		var versions []handlers.TemplateVersionSummary
		if response.StatusCode == fiber.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&versions); err != nil || len(versions) != 1 {
				t.Fatalf("roles %v: versions = %+v, %v", tc.roles, versions, err)
			}
		}
		if response.StatusCode != tc.want {
			t.Errorf("roles %v: GET versions = %d, want %d", tc.roles, response.StatusCode, tc.want)
			continue
		}

		versionID := template.ID.String() // unknown as a version when the list was refused
		if len(versions) == 1 {
			versionID = versions[0].ID.String()
		}
		response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/templates/"+template.ID.String()+"/versions/"+versionID, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != tc.want {
			t.Errorf("roles %v: GET one version = %d, want %d", tc.roles, response.StatusCode, tc.want)
		}
	}
}

// A version route never turns into a template route: /:id/versions sits
// beside /:id, and the unknown ids below get the API's 404 shape.
func TestTemplateVersionRoutesAnswerInTheAPIsErrorShape(t *testing.T) {
	app, template := templateRoutesApp(t, "skymail:access", "skymail:templates:read")

	for _, path := range []string{
		"/v1/templates/" + template.ID.String() + "/versions/" + template.ID.String(),
		"/v1/templates/0b8a4a53-5a1f-4b8e-9c55-2f7f0a3a9e10/versions",
	} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNotFound || body.Code != "server.not_found" {
			t.Errorf("GET %s = %d %+v, want 404 server.not_found", path, response.StatusCode, body)
		}
	}
}
