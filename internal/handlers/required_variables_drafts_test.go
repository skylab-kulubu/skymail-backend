package handlers

import (
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The editor's writes — saving a draft, restoring a version as one, and
// publishing — are held to the template's Required variables as the old
// panel's and the seed's are: the version's own body must reference each, as
// the sets stand when the write happens, or nothing is written.

const resetLinkReason = "Parola sıfırlama bağlantısı; kaldırılırsa mail işe yaramaz."

// resetPasswordWithRequirements seeds the password reset with link in its
// contract, with a reason, and has an operator mark firstName.
func resetPasswordWithRequirements(t *testing.T, app *fiber.App) requiredTemplate {
	t.Helper()
	status, body := seedResetPassword(t, app, resetPasswordBody, []map[string]any{{"name": "link", "reason": resetLinkReason}})
	if status != fiber.StatusOK {
		t.Fatalf("seed = %d %s", status, body)
	}
	seeded := templateOf(t, body)
	if status, body := addRequired(t, app, seeded.ID, "firstName"); status != fiber.StatusOK {
		t.Fatalf("add firstName = %d %s", status, body)
	}
	return readTemplate(t, app, seeded.ID)
}

// htmlDraft is a draft whose Main source is HTML, started from base.
func htmlDraft(html string, base *uuid.UUID) map[string]any {
	return map[string]any{
		"subject": "SKY LAB parola sıfırlama isteği", "main_mode": "html", "html_source": html,
		"html_content": html, "plain_text_content": "Parolanı sıfırla", "base_version_id": base,
	}
}

func assertMissing(t *testing.T, status int, body []byte, want []missingVariable) {
	t.Helper()
	if status != fiber.StatusUnprocessableEntity {
		t.Fatalf("status = %d %s, want 422", status, body)
	}
	if r := refusalOf(t, body); r.Code != "template.required_variables_missing" || !reflect.DeepEqual(r.Params.Missing, want) {
		t.Fatalf("refusal = %+v, want template.required_variables_missing naming %+v", r, want)
	}
}

// A draft that drops a Required variable is refused like a published write,
// naming the variable and why — the contract's reason, or an operator's
// mark — and records nothing. One that keeps them, inside an if or not, saves.
func TestADraftThatDropsARequiredVariableIsRefused(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)
	template := resetPasswordWithRequirements(t, app)
	base := publishedVersionID(t, db, template.ID)
	versions, _ := storedVersions(t, db, template.ID)

	for _, tc := range []struct {
		name string
		html string
		want []missingVariable
	}{
		{"the contract's link", `<p>{{.firstName}}</p><p>Parolanı sıfırla.</p>`, []missingVariable{{"link", "contract", reason(resetLinkReason)}}},
		{"the operator's firstName", `<a href="{{.link}}">Sıfırla</a>`, []missingVariable{{"firstName", "operator", nil}}},
		{"the link kept only in an HTML comment", `<p>{{.firstName}}</p><!-- <a href="{{.link}}">Sıfırla</a> -->`, []missingVariable{{"link", "contract", reason(resetLinkReason)}}},
		{"both", `<p>Parolanı sıfırla.</p>`, []missingVariable{{"firstName", "operator", nil}, {"link", "contract", reason(resetLinkReason)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, _, body := saveDraft(t, app, template.ID, htmlDraft(tc.html, &base))
			assertMissing(t, response.StatusCode, body, tc.want)
		})
	}
	if after, _ := storedVersions(t, db, template.ID); len(after) != len(versions) {
		t.Fatalf("a refused draft was recorded")
	}

	response, _, body := saveDraft(t, app, template.ID, htmlDraft(`{{if .firstName}}Merhaba {{.firstName}}{{end}} <a href="{{.link}}">Sıfırla</a>`, &base))
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("a draft that keeps both = %d %s, want 201", response.StatusCode, body)
	}
}

// Restoring writes a draft of an old version, checked against the sets as
// they are now: a version from before a variable became required cannot come
// back without it.
func TestRestoringAVersionThatLacksAVariableRequiredSinceIsRefused(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Etkinlik", "subject": "Etkinlik", "html_content": `<p>Merhaba {{.FullName}}</p>`,
		"plain_text_content": "Merhaba", "react_email_content": panelSource,
	})
	old := publishedVersionID(t, db, created.ID)
	response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": "Etkinlik", "subject": "Etkinlik", "html_content": `<p>Merhaba {{.FullName}}</p><a href="{{.EventUrl}}">Etkinlik</a>`,
		"plain_text_content": "Merhaba", "react_email_content": panelSource,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("edit = %d %s", response.StatusCode, body)
	}
	if status, body := addRequired(t, app, created.ID, "EventUrl"); status != fiber.StatusOK {
		t.Fatalf("add EventUrl = %d %s", status, body)
	}
	versions, _ := storedVersions(t, db, created.ID)

	response, _, body = restore(t, app, created.ID, old)
	assertMissing(t, response.StatusCode, body, []missingVariable{{"EventUrl", "operator", nil}})
	if after, _ := storedVersions(t, db, created.ID); len(after) != len(versions) {
		t.Fatalf("a refused restore was recorded")
	}

	// Released, the old version comes back.
	if status, body := removeRequired(t, app, created.ID, "EventUrl"); status != fiber.StatusOK {
		t.Fatalf("remove EventUrl = %d %s", status, body)
	}
	if response, _, body = restore(t, app, created.ID, old); response.StatusCode != fiber.StatusCreated {
		t.Fatalf("restore once released = %d %s, want 201", response.StatusCode, body)
	}
}

// A draft is checked again when it is published, against the sets as they
// stand then: one saved before an operator marked a variable it lacks cannot
// go out without it, and the row stays as it was.
func TestPublishingADraftChecksTheRequiredVariablesAsTheyAreThen(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Etkinlik", "subject": "Etkinlik", "html_content": `<p>Merhaba {{.FullName}}</p><a href="{{.EventUrl}}">Etkinlik</a>`,
		"plain_text_content": "Merhaba", "react_email_content": panelSource,
	})
	base := publishedVersionID(t, db, created.ID)
	response, draft, body := saveDraft(t, app, created.ID, map[string]any{
		"subject": "Etkinlik", "main_mode": "html", "html_source": `<p>Merhaba {{.FullName}}</p>`,
		"html_content": `<p>Merhaba {{.FullName}}</p>`, "plain_text_content": "Merhaba", "base_version_id": base,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft without EventUrl, nothing required yet = %d %s", response.StatusCode, body)
	}

	if status, body := addRequired(t, app, created.ID, "EventUrl"); status != fiber.StatusOK {
		t.Fatalf("add EventUrl = %d %s", status, body)
	}
	before := templateRow(t, db, created.ID)
	response, _, body = publish(t, app, created.ID, draft.ID, nil)
	assertMissing(t, response.StatusCode, body, []missingVariable{{"EventUrl", "operator", nil}})
	if after := templateRow(t, db, created.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused publish changed the row")
	}
	if versions, published := storedVersions(t, db, created.ID); published == nil || *published != base || versions[len(versions)-1].PublishedAt != nil {
		t.Fatalf("a refused publish published the draft")
	}

	if status, body := removeRequired(t, app, created.ID, "EventUrl"); status != fiber.StatusOK {
		t.Fatalf("remove EventUrl = %d %s", status, body)
	}
	if response, _, body = publish(t, app, created.ID, draft.ID, nil); response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish once released = %d %s, want 200", response.StatusCode, body)
	}
}

// A draft's own subject and parts are what is checked, not the row's.
func TestADraftIsCheckedByItsOwnSubjectAndBody(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := requiredVariablesApp(t, db)
	template := resetPasswordWithRequirements(t, app)
	base := publishedVersionID(t, db, template.ID)

	draft := htmlDraft(`{{.firstName}} <a href="{{.link}}">Sıfırla</a>`, &base)
	draft["subject"] = "{{.firstName parola"
	response, _, body := saveDraft(t, app, template.ID, draft)
	if r := refusalOf(t, body); response.StatusCode != fiber.StatusUnprocessableEntity || r.Code != "template.unparseable" || r.Params.Part != "subject" {
		t.Fatalf("draft with a broken subject = %d %s, want 422 template.unparseable of the subject", response.StatusCode, body)
	}
}

// publishedVersionID is the version a template's row is a copy of.
func publishedVersionID(t *testing.T, db *database.Store, id uuid.UUID) uuid.UUID {
	t.Helper()
	_, published := storedVersions(t, db, id)
	if published == nil {
		t.Fatal("no published version")
	}
	return *published
}
