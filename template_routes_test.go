package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// templateRoutesApp serves the template routes as production registers them —
// the real permission middleware, handler, error handler, body validator and
// database — with only Keycloak's token check replaced by the roles under
// test. It returns a
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

// sendJSON sends a JSON body to the app and reads the answer.
func sendJSON(t *testing.T, app *fiber.App, method, path string, body any) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

// Saving a draft, publishing and restoring a version are template writes:
// they take skymail:templates:write, as every other template write does, and
// no other role stands in for it.
func TestTemplateDraftWritesRequireTemplatesWrite(t *testing.T) {
	for _, tc := range []struct {
		roles   []string
		allowed bool
	}{
		{[]string{"skymail:access"}, false},
		{[]string{"skymail:access", "skymail:templates:read"}, false},
		{[]string{"skymail:access", "skymail:templates:read", "skymail:mails:write", "skymail:mails:send", "skymail:lists:write"}, false},
		{[]string{"skymail:access", "skymail:templates:write"}, true},
	} {
		app, template := templateRoutesApp(t, tc.roles...)
		base := template.ID.String() + "/versions/" + template.PublishedVersionID.String()

		status, body := sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+template.ID.String()+"/drafts", map[string]any{
			"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak</p>",
			"html_content": "<p>Taslak</p>", "plain_text_content": "Taslak", "base_version_id": template.PublishedVersionID,
		})
		var draft handlers.TemplateVersion
		if status == fiber.StatusCreated {
			if err := json.Unmarshal(body, &draft); err != nil {
				t.Fatal(err)
			}
		}
		checks := []struct {
			name   string
			status int
			want   int
		}{{"save a draft", status, fiber.StatusCreated}}

		draftPath := template.ID.String() + "/versions/" + draft.ID.String()
		status, _ = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+draftPath+"/publish", nil)
		checks = append(checks, struct {
			name   string
			status int
			want   int
		}{"publish", status, fiber.StatusOK})
		status, _ = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+base+"/restore", nil)
		checks = append(checks, struct {
			name   string
			status int
			want   int
		}{"restore", status, fiber.StatusCreated})

		for _, check := range checks {
			want := check.want
			if !tc.allowed {
				want = fiber.StatusForbidden
			}
			if check.status != want {
				t.Errorf("roles %v: %s = %d, want %d", tc.roles, check.name, check.status, want)
			}
		}
	}
}

// The draft routes refuse in the API's error shape, through the error
// handler production uses: a stale publish names the versions involved, an
// archived template says so, and an invalid body names its fields.
func TestTemplateDraftRefusalsAnswerInTheAPIsErrorShape(t *testing.T) {
	app, template := templateRoutesApp(t, "skymail:access", "skymail:templates:read", "skymail:templates:write")
	id := template.ID.String()
	type apiError struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Params  map[string]any `json:"params"`
	}
	decode := func(body []byte) apiError {
		t.Helper()
		var e apiError
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return e
	}

	status, body := sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+id+"/drafts", map[string]any{
		"subject": "", "main_mode": "html", "html_content": "<p>x</p>", "plain_text_content": "x",
	})
	if e := decode(body); status != fiber.StatusBadRequest || e.Code != "validation.error" || e.Params["errors"] == nil {
		t.Errorf("a draft with no subject = %d %s, want 400 validation.error listing the field", status, body)
	}

	status, body = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+id+"/drafts", map[string]any{
		"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak</p>",
		"html_content": "<p>Taslak</p>", "plain_text_content": "Taslak", "base_version_id": template.PublishedVersionID,
	})
	if status != fiber.StatusCreated {
		t.Fatalf("save = %d %s", status, body)
	}
	var draft handlers.TemplateVersion
	if err := json.Unmarshal(body, &draft); err != nil {
		t.Fatal(err)
	}
	status, body = sendJSON(t, app, fiber.MethodPatch, "/v1/templates/"+id, map[string]any{
		"name": template.Name, "subject": "Eski panelden", "html_content": "<p>Eski panelden</p>",
		"plain_text_content": "Eski panelden", "react_email_content": "{}",
	})
	if status != fiber.StatusOK {
		t.Fatalf("old panel edit = %d %s", status, body)
	}
	var edited struct {
		PublishedVersionID string `json:"published_version_id"`
	}
	if err := json.Unmarshal(body, &edited); err != nil {
		t.Fatal(err)
	}

	status, body = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+id+"/versions/"+draft.ID.String()+"/publish", nil)
	if e := decode(body); status != fiber.StatusConflict || e.Code != "template.stale_base" ||
		e.Params["version_id"] != draft.ID.String() || e.Params["base_version_id"] != template.PublishedVersionID.String() ||
		e.Params["published_version_id"] != edited.PublishedVersionID {
		t.Errorf("stale publish = %d %s, want 409 template.stale_base naming the draft, its base and %s", status, body, edited.PublishedVersionID)
	}

	if status, body := sendJSON(t, app, fiber.MethodDelete, "/v1/templates/"+id, nil); status != fiber.StatusNoContent {
		t.Fatalf("archive = %d %s", status, body)
	}
	status, body = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+id+"/versions/"+draft.ID.String()+"/publish", map[string]any{"force": map[string]any{"over_version_id": edited.PublishedVersionID}})
	if e := decode(body); status != fiber.StatusConflict || e.Code != "template.archived" {
		t.Errorf("publishing on an archived template = %d %s, want 409 template.archived", status, body)
	}

	status, body = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+uuid.NewString()+"/drafts", map[string]any{
		"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak</p>", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
	})
	if e := decode(body); status != fiber.StatusNotFound || e.Code != "server.not_found" {
		t.Errorf("a draft of an unknown template = %d %s, want 404 server.not_found", status, body)
	}
}
