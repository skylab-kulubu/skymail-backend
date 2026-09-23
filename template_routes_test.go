package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
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
// the real permission middleware, handler, error handler, body validator,
// JSON codec and database — with only Keycloak's token check replaced by the
// roles under test. It returns a template with one version to ask about.
func templateRoutesApp(t *testing.T, roles ...string) (*fiber.App, database.Template) {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(context.Background(), postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	template, err := store.PublishTemplateWrite(context.Background(), database.VersionAuthor{Kind: database.TemplateAuthorKindOperator},
		func(q *database.Queries) (database.Template, error) {
			return q.CreateTemplate(context.Background(), database.CreateTemplateParams{
				Name: "Bülten", Subject: "SKY LAB", HtmlContent: "<p>Merhaba {{.FullName}}</p>", PlainTextContent: "Merhaba {{.FullName}}", ReactEmailContent: "",
			})
		})
	if err != nil {
		t.Fatal(err)
	}

	app := fiber.New(fiber.Config{
		ErrorHandler:    errorHandler,
		StructValidator: validator.NewStructValidator(),
		JSONDecoder:     sonic.Unmarshal,
		JSONEncoder:     sonic.Marshal,
	})
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

// Saving a draft, publishing, restoring a version and discarding a draft are
// template writes: they take skymail:templates:write, as every other template
// write does, and no other role stands in for it.
func TestTemplateDraftWritesRequireTemplatesWrite(t *testing.T) {
	type check struct {
		name         string
		status, want int
	}
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
		versions := "/v1/templates/" + template.ID.String() + "/versions/"
		versionOf := func(status int, body []byte) handlers.TemplateVersion {
			t.Helper()
			var version handlers.TemplateVersion
			if status == fiber.StatusCreated {
				if err := json.Unmarshal(body, &version); err != nil {
					t.Fatal(err)
				}
			}
			return version
		}

		var checks []check
		status, body := sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+template.ID.String()+"/drafts", map[string]any{
			"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak</p>",
			"html_content": "<p>Taslak</p>", "plain_text_content": "Taslak", "base_version_id": template.PublishedVersionID,
		})
		draft := versionOf(status, body)
		checks = append(checks, check{"save a draft", status, fiber.StatusCreated})
		status, _ = sendJSON(t, app, fiber.MethodPost, versions+draft.ID.String()+"/publish", nil)
		checks = append(checks, check{"publish", status, fiber.StatusOK})
		status, body = sendJSON(t, app, fiber.MethodPost, versions+template.PublishedVersionID.String()+"/restore", nil)
		restored := versionOf(status, body)
		checks = append(checks, check{"restore", status, fiber.StatusCreated})
		status, _ = sendJSON(t, app, fiber.MethodPost, versions+restored.ID.String()+"/discard", nil)
		checks = append(checks, check{"discard", status, fiber.StatusOK})

		for _, c := range checks {
			want := c.want
			if !tc.allowed {
				want = fiber.StatusForbidden
			}
			if c.status != want {
				t.Errorf("roles %v: %s = %d, want %d", tc.roles, c.name, c.status, want)
			}
		}
	}
}

// The draft routes refuse in the API's error shape, through the error
// handler production uses: a stale publish names the versions involved, an
// archived template is not found, and an invalid body names its fields.
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
	if e := decode(body); status != fiber.StatusNotFound || e.Code != "server.not_found" {
		t.Errorf("publishing on an archived template = %d %s, want 404 server.not_found", status, body)
	}

	status, body = sendJSON(t, app, fiber.MethodPost, "/v1/templates/"+uuid.NewString()+"/drafts", map[string]any{
		"subject": "Taslak", "main_mode": "html", "html_source": "<p>Taslak</p>", "html_content": "<p>Taslak</p>", "plain_text_content": "Taslak",
	})
	if e := decode(body); status != fiber.StatusNotFound || e.Code != "server.not_found" {
		t.Errorf("a draft of an unknown template = %d %s, want 404 server.not_found", status, body)
	}
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

		if status, body := sendJSON(t, app, fiber.MethodPost, path, map[string]any{"name": "FullName"}); status != tc.want {
			t.Errorf("roles %v: POST = %d %s, want %d", tc.roles, status, body, tc.want)
		}
		if status, body := sendJSON(t, app, fiber.MethodDelete, path+"/FullName", nil); status != tc.want {
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
		}, "invalid_variable_name", "contract_required_variables[1].name"},
		{fiber.MethodPut, "/v1/templates/by-key/keycloak.reset-password", map[string]any{
			"name": "Parola", "subject": "Parola", "html_content": "<p>{{.link}}</p>", "plain_text_content": "{{.link}}",
			"react_email_content": "// kaynak", "system": true,
			"contract_required_variables": []any{map[string]any{"name": "link", "reason": strings.Repeat("uzun ", 61)}},
		}, "max_length", "contract_required_variables[0].reason"},
	} {
		status, body := sendJSON(t, app, tc.method, tc.path, tc.body)
		e := read(body)
		if status != fiber.StatusBadRequest || e.Code != "validation.error" || len(e.Params.Errors) != 1 ||
			e.Params.Errors[0].Code != tc.code || e.Params.Errors[0].Field != tc.field {
			t.Errorf("%s %s %v = %d %s, want 400 validation.error on %s (%s)", tc.method, tc.path, tc.body, status, body, tc.field, tc.code)
		}
	}

	status, body := sendJSON(t, app, fiber.MethodDelete, path+"/not-a-name", nil)
	if e := read(body); status != fiber.StatusBadRequest || e.Code != "template.invalid_variable_name" {
		t.Errorf("DELETE not-a-name = %d %s, want 400 template.invalid_variable_name", status, body)
	}
}

// The seed sends each contract variable with its reason; the seed on main
// today sends names alone. Production's JSON decoder takes both, in one list.
func TestTheSeedSendsContractVariablesWithOrWithoutReasons(t *testing.T) {
	app, _ := templateRoutesApp(t, "skymail:access", "skymail:templates:write", "skymail:templates:read")

	status, body := sendJSON(t, app, fiber.MethodPut, "/v1/templates/by-key/keycloak.reset-password", map[string]any{
		"name": "Parola", "subject": "Parola", "html_content": `<a href="{{.link}}">{{.firstName}}</a>`, "plain_text_content": "{{.link}}",
		"react_email_content": "// kaynak", "system": true,
		"contract_required_variables": []any{"firstName", map[string]any{"name": "link", "reason": "Parola sıfırlama bağlantısı."}},
	})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	var served struct {
		Contract []struct {
			Name   string  `json:"name"`
			Reason *string `json:"reason"`
		} `json:"contract_required_variables"`
		Operator []string `json:"operator_required_variables"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if len(served.Contract) != 2 || served.Contract[0].Name != "firstName" || served.Contract[0].Reason != nil ||
		served.Contract[1].Name != "link" || served.Contract[1].Reason == nil || *served.Contract[1].Reason != "Parola sıfırlama bağlantısı." ||
		served.Operator == nil || len(served.Operator) != 0 {
		t.Fatalf("served %s, want firstName without a reason and link with one, and no operator variables", body)
	}
}

// A refused seed answers through production's error handler and JSON codec in
// the API's error shape. The seed on main prints the first 200 bytes of an
// error body and stops, so those bytes alone must say what happened and how to
// force it. force passes through the route as a query parameter, and anything
// but a boolean there is refused like any invalid field.
func TestARefusedSeedAnswersInTheAPIsErrorShape(t *testing.T) {
	app, _ := templateRoutesApp(t, "skymail:access", "skymail:templates:write", "skymail:templates:read")
	const path = "/v1/templates/by-key/core.welcome"
	seed := func(html string) map[string]any {
		return map[string]any{
			"name": "Hoş Geldin", "subject": "Hoş geldin", "html_content": html, "plain_text_content": "Hoş geldin",
			"react_email_content": "// Kaynak: skymail-frontend/emails/core.welcome.tsx — burada düzenlersen repodaki kaynakla ayrışır.\n",
			"system":              true,
		}
	}
	status, body := sendJSON(t, app, fiber.MethodPut, path, seed("<p>İlk</p>"))
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}
	if status, body := sendJSON(t, app, fiber.MethodPatch, "/v1/templates/"+seeded.ID.String(), map[string]any{
		"name": seeded.Name, "subject": "Aramıza hoş geldin", "html_content": seeded.HtmlContent,
		"plain_text_content": seeded.PlainTextContent, "react_email_content": seeded.ReactEmailContent, "key": "core.welcome",
	}); status != fiber.StatusOK {
		t.Fatalf("operator edit = %d %s", status, body)
	}

	status, body = sendJSON(t, app, fiber.MethodPut, path, seed("<p>Koyu tema</p>"))
	var refused struct {
		Code   string `json:"code"`
		Params struct {
			Key        string   `json:"key"`
			TemplateID string   `json:"template_id"`
			Rules      []string `json:"rules"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("refused seed body %s: %v", body, err)
	}
	if status != fiber.StatusConflict || refused.Code != "template.seed_conflict" || refused.Params.Key != "core.welcome" ||
		refused.Params.TemplateID != seeded.ID.String() || len(refused.Params.Rules) == 0 {
		t.Fatalf("refused seed = %d %s, want 409 template.seed_conflict naming the template and its rules", status, body)
	}
	if head := string(body[:min(200, len(body))]); !strings.Contains(head, "template.seed_conflict") || !strings.Contains(head, "?force=true") {
		t.Fatalf("the first 200 bytes of the refusal, all main's seed prints, are %q: want the code and how to force", head)
	}

	if status, body := sendJSON(t, app, fiber.MethodPut, path+"?force=evet", seed("<p>Koyu tema</p>")); status != fiber.StatusBadRequest ||
		!strings.Contains(string(body), `"field":"force"`) {
		t.Fatalf("force=evet = %d %s, want 400 validation.error on force", status, body)
	}
	if status, body := sendJSON(t, app, fiber.MethodPut, path+"?force=true", seed("<p>Koyu tema</p>")); status != fiber.StatusOK {
		t.Fatalf("forced seed = %d %s, want 200", status, body)
	}
}
