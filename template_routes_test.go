package main

import (
	"bytes"
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
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// templateRoutesApp serves the template routes as production registers them —
// the real permission middleware, handler, validator, error handler and
// database — with only Keycloak's token check replaced by the roles under
// test. It returns a template with one version to ask about.
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
				Name: "Bülten", Subject: "SKY LAB", HtmlContent: "<p>Merhaba {{.FullName}}</p>", PlainTextContent: "Merhaba {{.FullName}}", ReactEmailContent: "",
			})
		})
	if err != nil {
		t.Fatal(err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
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

func sendRoute(t *testing.T, app *fiber.App, method, path string, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw.Bytes()
}

// The operator's Required variables are changed with the role every other
// template write needs.
func TestRequiredVariableRoutesRequireTemplatesWrite(t *testing.T) {
	for _, tc := range []struct {
		roles []string
		want  int
	}{
		{[]string{"skymail:access"}, fiber.StatusForbidden},
		{[]string{"skymail:access", "skymail:templates:read", "skymail:mails:write", "skymail:mails:send", "skymail:lists:write"}, fiber.StatusForbidden},
		{[]string{"skymail:access", "skymail:templates:write"}, fiber.StatusOK},
	} {
		app, template := templateRoutesApp(t, tc.roles...)
		path := "/v1/templates/" + template.ID.String() + "/required-variables"

		if status, body := sendRoute(t, app, fiber.MethodPost, path, map[string]any{"name": "FullName"}); status != tc.want {
			t.Errorf("roles %v: POST = %d %s, want %d", tc.roles, status, body, tc.want)
		}
		if status, body := sendRoute(t, app, fiber.MethodDelete, path+"/FullName", nil); status != tc.want {
			t.Errorf("roles %v: DELETE = %d %s, want %d", tc.roles, status, body, tc.want)
		}
	}
}

// A name that is not one a body could reach is the caller's mistake: a 400 in
// the API's error shape, from the body's validation or from the path.
func TestMalformedRequiredVariableNamesAreRefused(t *testing.T) {
	app, template := templateRoutesApp(t, "skymail:access", "skymail:templates:write")
	path := "/v1/templates/" + template.ID.String() + "/required-variables"

	type fieldError struct {
		Field string `json:"field"`
		Code  string `json:"code"`
	}
	type apiError struct {
		Code   string `json:"code"`
		Params struct {
			Errors []fieldError `json:"errors"`
		} `json:"params"`
	}
	read := func(body []byte) apiError {
		t.Helper()
		var e apiError
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("error body %s: %v", body, err)
		}
		return e
	}

	for _, tc := range []struct {
		method, path string
		body         any
		code, field  string
	}{
		{fiber.MethodPost, path, map[string]any{"name": "Full Name"}, "invalid_variable_name", "name"},
		{fiber.MethodPost, path, map[string]any{"name": "1st"}, "invalid_variable_name", "name"},
		{fiber.MethodPost, path, map[string]any{}, "required", "name"},
		{fiber.MethodPut, "/v1/templates/by-key/keycloak.reset-password", map[string]any{
			"name": "Parola", "subject": "Parola", "html_content": "<p>{{.link}}</p>", "plain_text_content": "{{.link}}",
			"react_email_content": "// kaynak", "system": true, "contract_required_variables": []string{"link", "reset link"},
		}, "invalid_variable_name", "contract_required_variables[1]"},
	} {
		status, body := sendRoute(t, app, tc.method, tc.path, tc.body)
		e := read(body)
		if status != fiber.StatusBadRequest || e.Code != "validation.error" || len(e.Params.Errors) != 1 ||
			e.Params.Errors[0].Code != tc.code || e.Params.Errors[0].Field != tc.field {
			t.Errorf("%s %s %v = %d %s, want 400 validation.error on %s (%s)", tc.method, tc.path, tc.body, status, body, tc.field, tc.code)
		}
	}

	status, body := sendRoute(t, app, fiber.MethodDelete, path+"/not-a-name", nil)
	if e := read(body); status != fiber.StatusBadRequest || e.Code != "template.invalid_variable_name" {
		t.Errorf("DELETE not-a-name = %d %s, want 400 template.invalid_variable_name", status, body)
	}
}
