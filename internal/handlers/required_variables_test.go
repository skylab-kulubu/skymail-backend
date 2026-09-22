package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// requiredVariablesApp is templateVersionsApp with the operator's Required
// variable routes beside the writers they constrain.
func requiredVariablesApp(t *testing.T, db *database.Store) *fiber.App {
	t.Helper()
	app := templateVersionsApp(t, db)
	templates := NewTemplateHandler(db)
	app.Post("/templates/:id/restore", templates.RestoreTemplate)
	app.Post("/templates/:id/required-variables", templates.AddRequiredVariable)
	app.Delete("/templates/:id/required-variables/:name", templates.RemoveRequiredVariable)
	return app
}

// A refusal as the client reads it: the code it translates, and the params
// that say which variable and why.
type refusal struct {
	Code   string `json:"code"`
	Params struct {
		Missing []missingVariable `json:"missing"`
		Error   string            `json:"error"`
		Name    string            `json:"name"`
	} `json:"params"`
}

type missingVariable struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

func refusalOf(t *testing.T, body []byte) refusal {
	t.Helper()
	var r refusal
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("refusal %s: %v", body, err)
	}
	return r
}

func templateOf(t *testing.T, body []byte) database.Template {
	t.Helper()
	assertTemplateShape(t, body)
	var template database.Template
	if err := json.Unmarshal(body, &template); err != nil {
		t.Fatalf("template %s: %v", body, err)
	}
	return template
}

func assertSets(t *testing.T, template database.Template, contract, operator []string) {
	t.Helper()
	if !reflect.DeepEqual(template.ContractRequiredVariables, contract) || !reflect.DeepEqual(template.OperatorRequiredVariables, operator) {
		t.Fatalf("Required variables = contract %q operator %q, want contract %q operator %q",
			template.ContractRequiredVariables, template.OperatorRequiredVariables, contract, operator)
	}
}

// resetPasswordSeed is the Template seed's payload for the password reset,
// the mail that cannot do its job without its link.
func resetPasswordSeed(html string, contract any) map[string]any {
	payload := map[string]any{
		"name": "Keycloak · Parola Sıfırlama", "subject": "SKY LAB parola sıfırlama isteği",
		"html_content": html, "plain_text_content": "Parolanı sıfırla",
		"react_email_content": seedPointerComment("keycloak.reset-password"), "system": true,
	}
	if contract != nil {
		payload["contract_required_variables"] = contract
	}
	return payload
}

const resetPasswordBody = `<p>{{if .firstName}}Merhaba {{.firstName}}, {{end}}parolanı sıfırla.</p><a href="{{.link}}">Yeni Parola Belirle</a>`

func seedResetPassword(t *testing.T, app *fiber.App, html string, contract any) (int, []byte) {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/keycloak.reset-password", resetPasswordSeed(html, contract))
	return response.StatusCode, body
}

func addRequired(t *testing.T, app *fiber.App, id uuid.UUID, name string) (int, []byte) {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+id.String()+"/required-variables", map[string]any{"name": name})
	return response.StatusCode, body
}

func removeRequired(t *testing.T, app *fiber.App, id uuid.UUID, name string) (int, []byte) {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodDelete, "/templates/"+id.String()+"/required-variables/"+name, nil)
	return response.StatusCode, body
}

// The contract set is the Template seed's: it arrives with the upsert and is
// served with the template. A seed that leaves it out leaves it as it is, so
// a seed from before contract sets keeps working; an empty list clears it.
func TestTheTemplateSeedWritesTheContractSet(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	status, body := seedResetPassword(t, app, resetPasswordBody, []string{"link"})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)
	assertSets(t, seeded, []string{"link"}, []string{})

	getResponse, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+seeded.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	var read database.Template
	if err := json.NewDecoder(getResponse.Body).Decode(&read); err != nil {
		t.Fatal(err)
	}
	assertSets(t, read, []string{"link"}, []string{})

	// The seed on main today sends no contract set.
	if status, body = seedResetPassword(t, app, resetPasswordBody, nil); status != fiber.StatusOK {
		t.Fatalf("seed without a contract set = %d %s", status, body)
	}
	assertSets(t, templateOf(t, body), []string{"link"}, []string{})

	// Written sorted, each name once.
	if status, body = seedResetPassword(t, app, resetPasswordBody, []string{"link", "firstName", "link"}); status != fiber.StatusOK {
		t.Fatalf("seed with a larger contract set = %d %s", status, body)
	}
	assertSets(t, templateOf(t, body), []string{"firstName", "link"}, []string{})

	if status, body = seedResetPassword(t, app, resetPasswordBody, []string{}); status != fiber.StatusOK {
		t.Fatalf("seed with an empty contract set = %d %s", status, body)
	}
	assertSets(t, templateOf(t, body), []string{}, []string{})
}

// A seed whose body drops a Required variable is refused like anyone else's
// write, naming the variable and the set it is in, and nothing of it is kept.
func TestASeedWhoseBodyDropsARequiredVariableIsRefused(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	status, body := seedResetPassword(t, app, resetPasswordBody, []string{"link"})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)
	if status, body := addRequired(t, app, seeded.ID, "firstName"); status != fiber.StatusOK {
		t.Fatalf("add firstName = %d %s", status, body)
	}

	for name, tc := range map[string]struct {
		contract any
		want     []missingVariable
	}{
		"the contract set it keeps": {nil, []missingVariable{{"firstName", "operator"}, {"link", "contract"}}},
		"the contract set it sends": {[]string{"link", "code"}, []missingVariable{{"code", "contract"}, {"firstName", "operator"}, {"link", "contract"}}},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := seedResetPassword(t, app, `<p>Bağlantısız bir gövde</p>`, tc.contract)
			if status != fiber.StatusUnprocessableEntity {
				t.Fatalf("seed = %d %s, want 422", status, body)
			}
			if r := refusalOf(t, body); r.Code != "template.required_variables_missing" || !reflect.DeepEqual(r.Params.Missing, tc.want) {
				t.Fatalf("refusal = %+v, want template.required_variables_missing naming %+v", r, tc.want)
			}
		})
	}

	row, err := db.GetTemplateById(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.HtmlContent != resetPasswordBody {
		t.Fatalf("a refused seed changed the body to %q", row.HtmlContent)
	}
	assertSets(t, row, []string{"link"}, []string{"firstName"})
	if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != 1 {
		t.Fatalf("versions = %d, want only the first seed's", len(versions))
	}
}

// A variable an operator marked that the contract comes to declare is the
// contract's from then on: it is locked, and the two sets never share a name.
func TestTheContractTakesOverAVariableAnOperatorMarked(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	status, body := seedResetPassword(t, app, resetPasswordBody, []string{"link"})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)
	if status, body := addRequired(t, app, seeded.ID, "firstName"); status != fiber.StatusOK {
		t.Fatalf("add firstName = %d %s", status, body)
	}

	if status, body = seedResetPassword(t, app, resetPasswordBody, []string{"firstName", "link"}); status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	assertSets(t, templateOf(t, body), []string{"firstName", "link"}, []string{})
}

// An operator marks further variables required and releases only those; the
// response is the template with both sets. Neither is a version: what a
// version holds did not change.
func TestAnOperatorAddsAndRemovesRequiredVariables(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Etkinlik duyurusu", "subject": "Duyuru",
		"html_content":        `<p>Merhaba {{.FullName}}</p>{{if .EventUrl}}<a href="{{.EventUrl}}">Etkinlik</a>{{end}}`,
		"plain_text_content":  "Merhaba {{.FullName}}",
		"react_email_content": panelSource,
	})
	assertSets(t, created, []string{}, []string{})

	steps := []struct {
		do       func() (int, []byte)
		operator []string
	}{
		{func() (int, []byte) { return addRequired(t, app, created.ID, "FullName") }, []string{"FullName"}},
		{func() (int, []byte) { return addRequired(t, app, created.ID, "FullName") }, []string{"FullName"}},
		// A reference inside a conditional section is a reference.
		{func() (int, []byte) { return addRequired(t, app, created.ID, "EventUrl") }, []string{"EventUrl", "FullName"}},
		{func() (int, []byte) { return removeRequired(t, app, created.ID, "FullName") }, []string{"EventUrl"}},
		// Releasing what is not required leaves things as they are.
		{func() (int, []byte) { return removeRequired(t, app, created.ID, "FullName") }, []string{"EventUrl"}},
	}
	for i, step := range steps {
		status, body := step.do()
		if status != fiber.StatusOK {
			t.Fatalf("step %d = %d %s", i, status, body)
		}
		assertSets(t, templateOf(t, body), []string{}, step.operator)
	}

	if versions, _ := storedVersions(t, db, created.ID); len(versions) != 1 {
		t.Fatalf("versions = %d, want only the create's", len(versions))
	}
}

// A contract variable is the sending service's: the operator's route does not
// release it, and asking to require it again changes nothing.
func TestTheOperatorsRouteLeavesTheContractSetAlone(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	status, body := seedResetPassword(t, app, resetPasswordBody, []string{"link"})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)

	status, body = removeRequired(t, app, seeded.ID, "link")
	if status != fiber.StatusConflict {
		t.Fatalf("remove link = %d %s, want 409", status, body)
	}
	if r := refusalOf(t, body); r.Code != "template.required_variable_in_contract" || r.Params.Name != "link" {
		t.Fatalf("refusal = %+v, want template.required_variable_in_contract naming link", r)
	}

	status, body = addRequired(t, app, seeded.ID, "link")
	if status != fiber.StatusOK {
		t.Fatalf("add link = %d %s", status, body)
	}
	assertSets(t, templateOf(t, body), []string{"link"}, []string{})

	row, err := db.GetTemplateById(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSets(t, row, []string{"link"}, []string{})
}

// A variable is required of the mail being sent, so it can only be marked
// when the published body references it — counted by the shared rule.
func TestAnOperatorCannotRequireAVariableThePublishedBodyDoesNotReference(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Bilet", "subject": "Biletin",
		"html_content": `<p>{{/* .Commented */}}Plain .Text</p>` +
			`{{range .Tickets}}<a href="{{.QrUrl}}">{{$.EventName}}</a>{{end}}{{index . "Indexed"}}`,
		"plain_text_content":  "Biletin",
		"react_email_content": panelSource,
	})

	for _, name := range []string{"Missing", "Commented", "Text", "QrUrl", "Indexed"} {
		status, body := addRequired(t, app, created.ID, name)
		if status != fiber.StatusUnprocessableEntity {
			t.Fatalf("add %s = %d %s, want 422", name, status, body)
		}
		if r := refusalOf(t, body); r.Code != "template.required_variables_missing" || !reflect.DeepEqual(r.Params.Missing, []missingVariable{{name, "operator"}}) {
			t.Fatalf("add %s refusal = %+v", name, r)
		}
	}
	for _, name := range []string{"Tickets", "EventName"} {
		if status, body := addRequired(t, app, created.ID, name); status != fiber.StatusOK {
			t.Fatalf("add %s = %d %s", name, status, body)
		}
	}

	row, err := db.GetTemplateById(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSets(t, row, []string{}, []string{"EventName", "Tickets"})
}

// Every save is checked the same way: a body that no longer references a
// Required variable is refused with its name and set, and nothing is written.
// What counts as a reference is the shared rule; these are its edges as a
// save meets them.
func TestAPanelEditIsCheckedAgainstTheRequiredVariables(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	status, body := seedResetPassword(t, app, resetPasswordBody, []string{"link"})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)
	if status, body := addRequired(t, app, seeded.ID, "firstName"); status != fiber.StatusOK {
		t.Fatalf("add firstName = %d %s", status, body)
	}

	edit := func(html string) (int, []byte) {
		response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), map[string]any{
			"name": seeded.Name, "subject": seeded.Subject, "key": "keycloak.reset-password",
			"html_content": html, "plain_text_content": "Parolanı sıfırla",
			"react_email_content": seeded.ReactEmailContent,
		})
		return response.StatusCode, body
	}

	for _, tc := range []struct {
		name    string
		html    string
		missing []missingVariable
	}{
		{"both inside conditional sections", `{{if .firstName}}{{.firstName}}{{end}}{{if .link}}<a href="{{.link}}">Sıfırla</a>{{end}}`, nil},
		{"$ inside a range", `{{range .Steps}}{{$.firstName}} <a href="{{$.link}}">{{.Label}}</a>{{end}}`, nil},
		{"the link only in a comment", `<p>{{.firstName}}</p>{{/* <a href="{{.link}}">Sıfırla</a> */}}`, []missingVariable{{"link", "contract"}}},
		{"the link only as text", `<p>{{.firstName}}</p><p>.link {.link} link</p>`, []missingVariable{{"link", "contract"}}},
		{"the link as a range element's field", `<p>{{.firstName}}</p>{{range .Links}}<a href="{{.link}}">Sıfırla</a>{{end}}`, []missingVariable{{"link", "contract"}}},
		{"the link as a with's field", `<p>{{.firstName}}</p>{{with .Account}}<a href="{{.link}}">Sıfırla</a>{{end}}`, []missingVariable{{"link", "contract"}}},
		{"the link through index", `<p>{{.firstName}}</p><a href="{{index . "link"}}">Sıfırla</a>`, []missingVariable{{"link", "contract"}}},
		{"neither", `<p>Parolanı sıfırla.</p>`, []missingVariable{{"firstName", "operator"}, {"link", "contract"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := db.GetTemplateById(context.Background(), seeded.ID)
			if err != nil {
				t.Fatal(err)
			}
			versionsBefore, _ := storedVersions(t, db, seeded.ID)

			status, body := edit(tc.html)
			if tc.missing == nil {
				if status != fiber.StatusOK {
					t.Fatalf("edit = %d %s, want 200", status, body)
				}
				return
			}
			if status != fiber.StatusUnprocessableEntity {
				t.Fatalf("edit = %d %s, want 422", status, body)
			}
			if r := refusalOf(t, body); r.Code != "template.required_variables_missing" || !reflect.DeepEqual(r.Params.Missing, tc.missing) {
				t.Fatalf("refusal = %+v, want template.required_variables_missing naming %+v", r, tc.missing)
			}
			after, err := db.GetTemplateById(context.Background(), seeded.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.HtmlContent != before.HtmlContent || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("a refused edit changed the row: %q", after.HtmlContent)
			}
			if versionsAfter, _ := storedVersions(t, db, seeded.ID); len(versionsAfter) != len(versionsBefore) {
				t.Fatalf("a refused edit wrote a version")
			}
		})
	}
}

// A body the mailer cannot parse could not be sent at all, and the check could
// not say what it references. Every writer is refused with its own error,
// carrying the parser's words, and nothing is written.
func TestABodyThatIsNotAGoTemplateIsRefused(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	assertUnparseable := func(t *testing.T, status int, body []byte) {
		t.Helper()
		if status != fiber.StatusUnprocessableEntity {
			t.Fatalf("status = %d %s, want 422", status, body)
		}
		if r := refusalOf(t, body); r.Code != "template.body_unparseable" || r.Params.Error == "" {
			t.Fatalf("refusal = %+v, want template.body_unparseable with the parser's error", r)
		}
	}

	response, body := sendJSON(t, app, fiber.MethodPost, "/templates", map[string]any{
		"name": "Kırık", "subject": "Kırık", "html_content": `<a href="{{.link">Git</a>`,
		"plain_text_content": "Kırık", "react_email_content": panelSource,
	})
	assertUnparseable(t, response.StatusCode, body)
	if count, err := db.CountAllTemplatesIncludingArchived(context.Background()); err != nil || count != 0 {
		t.Fatalf("templates after a refused create = %d, %v", count, err)
	}

	created := createTemplate(t, app, map[string]any{
		"name": "Sağlam", "subject": "Sağlam", "html_content": `<p>{{.FullName}}</p>`,
		"plain_text_content": "Sağlam", "react_email_content": panelSource,
	})
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": "Sağlam", "subject": "Sağlam", "html_content": `{{if .FullName}}<p>{{.FullName}}</p>`,
		"plain_text_content": "Sağlam", "react_email_content": panelSource,
	})
	assertUnparseable(t, response.StatusCode, body)

	status, body := seedResetPassword(t, app, `<a href="{{shout .link}}">Sıfırla</a>`, []string{"link"})
	assertUnparseable(t, status, body)
	if _, err := db.GetTemplateByKey(context.Background(), ptr("keycloak.reset-password")); err == nil {
		t.Fatal("a refused seed created its template")
	}

	if row, err := db.GetTemplateById(context.Background(), created.ID); err != nil || row.HtmlContent != `<p>{{.FullName}}</p>` {
		t.Fatalf("a refused edit changed the row: %q %v", row.HtmlContent, err)
	}
}

// The operator's routes change a template in use; an archived one is restored
// first, like any other edit.
func TestRequiredVariablesOfAnArchivedOrUnknownTemplate(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Arşivlik", "subject": "Arşivlik", "html_content": `<p>{{.FullName}}</p>`,
		"plain_text_content": "Arşivlik", "react_email_content": panelSource,
	})
	if response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+created.ID.String(), nil)); err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("archive: %v %v", response, err)
	}

	for _, id := range []string{created.ID.String(), uuid.NewString(), "not-a-uuid"} {
		response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+id+"/required-variables", map[string]any{"name": "FullName"})
		if response.StatusCode != fiber.StatusNotFound {
			t.Errorf("add on %s = %d %s, want 404", id, response.StatusCode, body)
		}
		response, body = sendJSON(t, app, fiber.MethodDelete, "/templates/"+id+"/required-variables/FullName", nil)
		if response.StatusCode != fiber.StatusNotFound {
			t.Errorf("remove on %s = %d %s, want 404", id, response.StatusCode, body)
		}
	}
}

func ptr(s string) *string { return &s }
