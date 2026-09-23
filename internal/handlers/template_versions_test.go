package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// The operator every request in these tests comes from: the Keycloak subject
// and the name the token carried.
const (
	operatorSub  = "5b0c3a4e-8f0e-4d7a-9a51-0c7b7f3f2a11"
	operatorName = "Ada Yılmaz"
)

// A React Email source as the old panel saves it.
const panelSource = `import { Html, Body, Text } from '@react-email/components';

export default function Email() {
  return (
    <Html>
      <Body>
        <Text>Merhaba {"{{.FullName}}"}</Text>
      </Body>
    </Html>
  );
}
`

func seedPointerComment(key string) string {
	return "// Kaynak: skymail-frontend/emails/" + key + ".tsx — burada düzenlersen repodaki kaynakla ayrışır.\n"
}

// templateVersionsApp serves the template routes and a single send the way
// the old panel, the new editor, the Template seed and a sending service reach
// them, with the real mailer queueing into the test database and request
// bodies validated as production validates them.
//
// Requests come from operatorSub unless they carry X-Operator-Sub (and
// X-Operator-Name): another operator's token.
func templateVersionsApp(t *testing.T, db *database.Store) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{StructValidator: validator.NewStructValidator(), ErrorHandler: func(c fiber.Ctx, err error) error {
		var appErr *apperrors.AppError
		if errors.As(err, &appErr) {
			return c.Status(appErr.Status).JSON(appErr)
		}
		var invalid validator.ValidationErrors
		if errors.As(err, &invalid) {
			return c.Status(fiber.StatusBadRequest).JSON(apperrors.ErrValidation.WithParams(map[string]interface{}{
				"errors": validator.ParseValidationErrors(invalid),
			}))
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}})
	app.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", operatorSub)
		c.Locals("user_name", operatorName)
		if sub := c.Get("X-Operator-Sub"); sub != "" {
			c.Locals("user_id", sub)
			c.Locals("user_name", c.Get("X-Operator-Name"))
		}
		return c.Next()
	})

	templates := NewTemplateHandler(db)
	app.Post("/templates", templates.CreateTemplate)
	app.Get("/templates", templates.GetTemplates)
	app.Get("/templates/:id", templates.GetTemplate)
	app.Patch("/templates/:id", templates.UpdateTemplate)
	app.Delete("/templates/:id", templates.DeleteTemplate)
	app.Post("/templates/:id/restore", templates.RestoreTemplate)
	app.Get("/templates/by-key/:key", templates.GetTemplateByKey)
	app.Put("/templates/by-key/:key", templates.UpsertTemplateByKey)
	app.Get("/templates/:id/versions", templates.ListTemplateVersions)
	app.Get("/templates/:id/versions/:versionId", templates.GetTemplateVersion)
	app.Post("/templates/:id/drafts", templates.SaveTemplateDraft)
	app.Post("/templates/:id/versions/:versionId/publish", templates.PublishTemplateVersion)
	app.Post("/templates/:id/versions/:versionId/restore", templates.RestoreTemplateVersion)

	mails := NewMailHandler(db, mailer.NewMailer(db, mailer.SMTPConfig{}), lifecycleKeycloakStub{})
	app.Post("/mail_tasks/single", mails.SendSingle)
	return app
}

func sendJSON(t *testing.T, app *fiber.App, method, path string, body any) (*http.Response, []byte) {
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
	return response, raw
}

// storedVersion is a version as the database holds it.
type storedVersion struct {
	ID               uuid.UUID
	Seq              int
	Subject          string
	RequestedSubject *string
	JSXSource        *string
	VisualSource     []byte
	HTMLSource       *string
	MainMode         string
	HTMLContent      string
	PlainTextContent string
	AuthorKind       string
	AuthorSub        *string
	AuthorName       *string
	CreatedAt        time.Time
	PublishedAt      *time.Time
	BaseVersionID    *uuid.UUID
}

// storedVersions reads a template's versions oldest first, straight from the
// database, and which of them the row says it is a copy of.
func storedVersions(t *testing.T, db *database.Store, templateID uuid.UUID) ([]storedVersion, *uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Conn.Query(ctx, `
		SELECT id, seq, subject, requested_subject, jsx_source, visual_source, html_source, main_mode::text, html_content,
		       plain_text_content, author_kind::text, author_sub, author_name, created_at, published_at, base_version_id
		FROM template_versions
		WHERE template_id = $1
		ORDER BY seq`, templateID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []storedVersion
	for rows.Next() {
		var v storedVersion
		if err := rows.Scan(&v.ID, &v.Seq, &v.Subject, &v.RequestedSubject, &v.JSXSource, &v.VisualSource, &v.HTMLSource, &v.MainMode, &v.HTMLContent,
			&v.PlainTextContent, &v.AuthorKind, &v.AuthorSub, &v.AuthorName, &v.CreatedAt, &v.PublishedAt, &v.BaseVersionID); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var published *uuid.UUID
	if err := db.Conn.QueryRow(ctx, `SELECT published_version_id FROM templates WHERE id = $1`, templateID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	return versions, published
}

// The fields a template is served with: the ones the old panel and core have
// always read, and beside them, all ignored by the old panel,
// published_version_id — the version the row is a copy of, which the new
// editor starts a draft from — main_mode, the Authoring mode of the Main
// source it sends, and drafts, each operator's draft in progress.
var templateFields = []string{
	"archived_at", "archived_by", "created_at", "drafts", "html_content", "id", "key", "main_mode", "name",
	"plain_text_content", "published_version_id", "react_email_content", "subject", "system", "updated_at",
}

func assertTemplateShape(t *testing.T, body []byte) {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("template response %s: %v", body, err)
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	if len(fields) != len(templateFields) {
		t.Fatalf("template served with %v, want %v", fields, templateFields)
	}
	for i := range fields {
		if fields[i] != templateFields[i] {
			t.Fatalf("template served with %v, want %v", fields, templateFields)
		}
	}
}

func sameString(a *string, b string) bool { return a != nil && *a == b }

// The old panel's create goes on working as it did, and what it created is now
// also the template's first version: an operator's, published at once, since
// the old panel has no drafts.
func TestOldPanelCreateWritesAPublishedOperatorVersion(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	response, body := sendJSON(t, app, fiber.MethodPost, "/templates", map[string]any{
		"name": "Etkinlik duyurusu", "subject": "Merhaba {{.FullName}}",
		"html_content": "<p>Merhaba {{.FullName}}</p>", "plain_text_content": "Merhaba {{.FullName}}",
		"react_email_content": panelSource,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("POST /templates = %d %s", response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var created database.Template
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Subject != "Merhaba {{.FullName}}" || created.HtmlContent != "<p>Merhaba {{.FullName}}</p>" ||
		created.PlainTextContent != "Merhaba {{.FullName}}" || created.ReactEmailContent != panelSource {
		t.Fatalf("created row = %+v, want the request's content", created)
	}

	versions, published := storedVersions(t, db, created.ID)
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	v := versions[0]
	if v.Seq != 1 || v.PublishedAt == nil || v.BaseVersionID != nil || published == nil || *published != v.ID {
		t.Fatalf("version = seq %d published %v base %v, row copy of %v; want the first version, published, the row's", v.Seq, v.PublishedAt, v.BaseVersionID, published)
	}
	if created.PublishedVersionID == nil || *created.PublishedVersionID != v.ID {
		t.Fatalf("response published_version_id = %v, want the version just written (%s)", created.PublishedVersionID, v.ID)
	}
	if v.RequestedSubject != nil {
		t.Fatalf("an operator's version has requested_subject %q; only a Template seed asks for one", *v.RequestedSubject)
	}
	if v.AuthorKind != "operator" || !sameString(v.AuthorSub, operatorSub) || !sameString(v.AuthorName, operatorName) {
		t.Fatalf("author = %s %v %v, want the operator by subject and name", v.AuthorKind, v.AuthorSub, v.AuthorName)
	}
	if v.MainMode != "jsx" || !sameString(v.JSXSource, panelSource) || v.HTMLSource != nil || v.VisualSource != nil {
		t.Fatalf("sources = main %s jsx %v html %v visual %s, want the panel's JSX as the only and Main source", v.MainMode, v.JSXSource != nil, v.HTMLSource, v.VisualSource)
	}
	if v.Subject != created.Subject || v.HTMLContent != created.HtmlContent || v.PlainTextContent != created.PlainTextContent {
		t.Fatalf("version content = %q %q %q, want the row's", v.Subject, v.HTMLContent, v.PlainTextContent)
	}
}

func createTemplate(t *testing.T, app *fiber.App, body map[string]any) database.Template {
	t.Helper()
	response, raw := sendJSON(t, app, fiber.MethodPost, "/templates", body)
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("POST /templates = %d %s", response.StatusCode, raw)
	}
	var created database.Template
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	return created
}

// An edit in the old panel still changes the row the way it always has, and
// is now an operator's version too — published at once, started from the
// version the row was a copy of.
func TestOldPanelEditWritesAPublishedOperatorVersion(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Etkinlik duyurusu", "subject": "Merhaba {{.FullName}}",
		"html_content": "<p>Merhaba {{.FullName}}</p>", "plain_text_content": "Merhaba {{.FullName}}",
		"react_email_content": panelSource,
	})

	edited := panelSource + "\n// ikinci paragraf eklendi\n"
	response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": "Etkinlik duyurusu", "subject": "Selam {{.FullName}}",
		"html_content": "<p>Selam {{.FullName}}</p><p>İkinci</p>", "plain_text_content": "Selam {{.FullName}} İkinci",
		"react_email_content": edited,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH = %d %s", response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var updated database.Template
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Subject != "Selam {{.FullName}}" || updated.HtmlContent != "<p>Selam {{.FullName}}</p><p>İkinci</p>" ||
		updated.PlainTextContent != "Selam {{.FullName}} İkinci" || updated.ReactEmailContent != edited {
		t.Fatalf("edited row = %+v, want the request's content", updated)
	}

	versions, published := storedVersions(t, db, created.ID)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	first, second := versions[0], versions[1]
	if second.Seq != 2 || second.PublishedAt == nil || published == nil || *published != second.ID {
		t.Fatalf("edit = seq %d published %v, row copy of %v; want version 2, published, the row's", second.Seq, second.PublishedAt, published)
	}
	if second.BaseVersionID == nil || *second.BaseVersionID != first.ID {
		t.Fatalf("edit's base = %v, want the version it replaced (%s)", second.BaseVersionID, first.ID)
	}
	if updated.PublishedVersionID == nil || *updated.PublishedVersionID != second.ID {
		t.Fatalf("response published_version_id = %v, want the edit's version (%s)", updated.PublishedVersionID, second.ID)
	}
	if second.AuthorKind != "operator" || !sameString(second.AuthorSub, operatorSub) || !sameString(second.AuthorName, operatorName) {
		t.Fatalf("author = %s %v %v, want the operator", second.AuthorKind, second.AuthorSub, second.AuthorName)
	}
	if second.MainMode != "jsx" || !sameString(second.JSXSource, edited) || second.Subject != updated.Subject ||
		second.HTMLContent != updated.HtmlContent || second.PlainTextContent != updated.PlainTextContent {
		t.Fatalf("edit version = %+v, want the row's content with its JSX as the Main source", second)
	}
	if first.Subject != "Merhaba {{.FullName}}" || !sameString(first.JSXSource, panelSource) {
		t.Fatalf("the first version changed: %+v", first)
	}
}

// The old panel cannot render the seed's pointer comment, so it saves a
// reworded seeded template with the stored body and the pointer sent back
// unchanged. That operator's version has HTML as its Main source, like the
// seed's version before it.
func TestOldPanelRewordingASeededTemplateKeepsHTMLAsTheMainSource(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	const key = "keycloak.verify-email"
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "E-posta doğrulama", "subject": "E-postanı doğrula",
		"html_content": `<a href="{{.link}}">Doğrula</a>`, "plain_text_content": "Doğrula: {{.link}}",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}

	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), map[string]any{
		"name": seeded.Name, "subject": "E-posta adresini doğrula", "key": key,
		"html_content": seeded.HtmlContent, "plain_text_content": seeded.PlainTextContent,
		"react_email_content": seeded.ReactEmailContent,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH = %d %s", response.StatusCode, body)
	}

	versions, _ := storedVersions(t, db, seeded.ID)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	reworded := versions[1]
	if reworded.AuthorKind != "operator" || reworded.Subject != "E-posta adresini doğrula" {
		t.Fatalf("rewording = %s %q, want the operator's subject", reworded.AuthorKind, reworded.Subject)
	}
	if reworded.MainMode != "html" || reworded.JSXSource != nil || !sameString(reworded.HTMLSource, seeded.HtmlContent) {
		t.Fatalf("rewording sources = main %s jsx %v html %v, want HTML as the Main source holding the stored body", reworded.MainMode, reworded.JSXSource, reworded.HTMLSource)
	}
}

// A refused edit writes no version: the row and its history stay in step.
func TestRefusedOldPanelEditWritesNoVersion(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	archived := createTemplate(t, app, map[string]any{
		"name": "Eski", "subject": "Eski", "html_content": "<p>Eski</p>", "plain_text_content": "Eski",
		"react_email_content": panelSource,
	})
	if response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+archived.ID.String(), nil)); err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("archive: %v %v", response, err)
	}
	response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+archived.ID.String(), map[string]any{
		"name": "Eski", "subject": "Yeni", "html_content": "<p>Yeni</p>", "plain_text_content": "Yeni",
		"react_email_content": panelSource,
	})
	if response.StatusCode != fiber.StatusNotFound {
		t.Fatalf("editing an archived template = %d %s, want 404", response.StatusCode, body)
	}
	if versions, _ := storedVersions(t, db, archived.ID); len(versions) != 1 {
		t.Fatalf("versions after a refused edit = %d, want 1", len(versions))
	}

	system := seedSystemTemplate(t, db, "keycloak.reset-password")
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+system.ID.String(), map[string]any{
		"name": system.Name, "subject": "Yeni", "html_content": "<p>x</p>", "plain_text_content": "x",
		"react_email_content": panelSource, "key": "keycloak.something-else",
	})
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("renaming a System template's key = %d %s, want 409", response.StatusCode, body)
	}
	if versions, _ := storedVersions(t, db, system.ID); len(versions) != 0 {
		t.Fatalf("versions after a refused key change = %d, want none", len(versions))
	}
}

// The Template seed's by-key upsert writes a Template seed version, published
// at once, of what the row ends up holding. Until ticket 09 the upsert keeps
// an operator's subject on an existing key (#18), so the seed's version carries
// the kept subject, not the one in the seed's payload: the version is what is
// sent.
func TestUpsertByKeyWritesAPublishedSeedVersionOfTheResultingRow(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	const key = "keycloak.personal-email-confirm"
	seed := map[string]any{
		"name": "Kişisel e-posta doğrulama", "subject": "E-postanı doğrula",
		"html_content": `<a href="{{.link}}">ilk gövde</a>`, "plain_text_content": "ilk gövde {{.link}}",
		"react_email_content": seedPointerComment(key), "system": true,
	}
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, seed)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("first seed = %d %s", response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}

	versions, published := storedVersions(t, db, seeded.ID)
	if len(versions) != 1 {
		t.Fatalf("versions after the first seed = %d, want 1", len(versions))
	}
	first := versions[0]
	if first.AuthorKind != "template_seed" || !sameString(first.AuthorSub, operatorSub) || !sameString(first.AuthorName, operatorName) {
		t.Fatalf("seed author = %s %v %v, want a Template seed, run with the caller's token", first.AuthorKind, first.AuthorSub, first.AuthorName)
	}
	if !sameString(first.RequestedSubject, "E-postanı doğrula") {
		t.Fatalf("first seed requested_subject = %v, want the subject it sent", first.RequestedSubject)
	}
	if first.PublishedAt == nil || first.BaseVersionID != nil || published == nil || *published != first.ID {
		t.Fatalf("seed version published %v base %v, row copy of %v; want published, no base, the row's", first.PublishedAt, first.BaseVersionID, published)
	}
	if first.MainMode != "html" || first.JSXSource != nil || !sameString(first.HTMLSource, seed["html_content"].(string)) ||
		first.Subject != "E-postanı doğrula" || first.HTMLContent != seed["html_content"] || first.PlainTextContent != seed["plain_text_content"] {
		t.Fatalf("seed version = %+v, want the pointer read as no JSX source and the body as the HTML Main source", first)
	}

	// An operator rewords the subject in the old panel…
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), map[string]any{
		"name": seeded.Name, "subject": "E-posta adresini doğrula", "key": key,
		"html_content": seeded.HtmlContent, "plain_text_content": seeded.PlainTextContent,
		"react_email_content": seeded.ReactEmailContent,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("reword = %d %s", response.StatusCode, body)
	}

	// …and the seed runs again with a body fix and the repo's subject.
	seed["html_content"] = `<a href="{{.link}}">ikinci gövde</a>`
	seed["plain_text_content"] = "ikinci gövde {{.link}}"
	response, body = sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, seed)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("second seed = %d %s", response.StatusCode, body)
	}
	assertTemplateShape(t, body)
	var reseeded database.Template
	if err := json.Unmarshal(body, &reseeded); err != nil {
		t.Fatal(err)
	}
	// The response the seed reads its "konu korundu" line from is unchanged.
	if reseeded.ID != seeded.ID || reseeded.Subject != "E-posta adresini doğrula" {
		t.Fatalf("reseed answered %s %q, want the same template with the operator's subject", reseeded.ID, reseeded.Subject)
	}

	versions, published = storedVersions(t, db, seeded.ID)
	if len(versions) != 3 {
		t.Fatalf("versions = %d, want seed, operator, seed", len(versions))
	}
	operator, reseed := versions[1], versions[2]
	if reseed.AuthorKind != "template_seed" || reseed.PublishedAt == nil || published == nil || *published != reseed.ID {
		t.Fatalf("reseed version = %s published %v, row copy of %v; want a published Template seed version, the row's", reseed.AuthorKind, reseed.PublishedAt, published)
	}
	if reseed.BaseVersionID == nil || *reseed.BaseVersionID != operator.ID {
		t.Fatalf("reseed base = %v, want the operator's version it replaced", reseed.BaseVersionID)
	}
	if reseed.Subject != "E-posta adresini doğrula" {
		t.Fatalf("reseed version subject = %q, want the subject the row kept", reseed.Subject)
	}
	// What the seed asked for is kept beside what it got, so ticket 09 can tell
	// a subject the seed did not set.
	if !sameString(reseed.RequestedSubject, "E-postanı doğrula") {
		t.Fatalf("reseed requested_subject = %v, want the repo's subject it sent", reseed.RequestedSubject)
	}
	if operator.RequestedSubject != nil {
		t.Fatalf("the operator's version has requested_subject %q", *operator.RequestedSubject)
	}
	if reseeded.PublishedVersionID == nil || *reseeded.PublishedVersionID != reseed.ID {
		t.Fatalf("response published_version_id = %v, want the reseed's version", reseeded.PublishedVersionID)
	}
	if reseed.HTMLContent != seed["html_content"] || reseed.PlainTextContent != seed["plain_text_content"] || !sameString(reseed.HTMLSource, seed["html_content"].(string)) {
		t.Fatalf("reseed version body = %q %q, want the repo's new body", reseed.HTMLContent, reseed.PlainTextContent)
	}
}

// A seed brings an archived template back. Restoring changes only the row, so
// an unchanged seed records no version; a changed one does.
func TestUpsertByKeyOfAnArchivedTemplate(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	payload := map[string]any{
		"name": "Serbest duyuru", "subject": "{{.Subject}}",
		"html_content": "{{safeHTML .Body}}", "plain_text_content": "{{.Body}}",
		"react_email_content": seedPointerComment("free.basic"),
	}
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/free.basic", payload)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}
	archive := func() {
		t.Helper()
		if response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+seeded.ID.String(), nil)); err != nil || response.StatusCode != fiber.StatusNoContent {
			t.Fatalf("archive: %v %v", response, err)
		}
	}

	archive()
	if response, body = sendJSON(t, app, fiber.MethodPut, "/templates/by-key/free.basic", payload); response.StatusCode != fiber.StatusOK {
		t.Fatalf("unchanged reseed = %d %s", response.StatusCode, body)
	}
	if _, err := db.GetTemplateById(context.Background(), seeded.ID); err != nil {
		t.Fatalf("the reseed left the template archived: %v", err)
	}
	if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != 1 {
		t.Fatalf("versions after an unchanged reseed = %d, want 1", len(versions))
	}

	archive()
	payload["html_content"] = "<div>{{safeHTML .Body}}</div>"
	if response, body = sendJSON(t, app, fiber.MethodPut, "/templates/by-key/free.basic", payload); response.StatusCode != fiber.StatusOK {
		t.Fatalf("changed reseed = %d %s", response.StatusCode, body)
	}
	versions, published := storedVersions(t, db, seeded.ID)
	if len(versions) != 2 || versions[1].AuthorKind != "template_seed" || published == nil || *published != versions[1].ID {
		t.Fatalf("versions = %+v, want a second, published Template seed version", versions)
	}
}

// The send path reads the row, and the row is a copy of the published version.
// After the old panel and the seed have written versions — and with a draft
// newer than all of them lying in the store — a send still queues exactly the
// published content.
func TestSendAfterVersionedWritesQueuesThePublishedCopyNotADraft(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	ctx := context.Background()

	const key = "core.welcome"
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": "<p>Hoş geldin {{.FullName}}</p>", "plain_text_content": "Hoş geldin {{.FullName}}",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var seeded database.Template
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatal(err)
	}
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), map[string]any{
		"name": seeded.Name, "subject": "Aramıza hoş geldin, {{.FullName}}", "key": key,
		"html_content": "<p>Aramıza hoş geldin {{.FullName}}</p>", "plain_text_content": "Aramıza hoş geldin {{.FullName}}",
		"react_email_content": panelSource,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("edit = %d %s", response.StatusCode, body)
	}

	// A draft as ticket 07 will write one: newer, unpublished, not on the row.
	if _, err := db.Conn.Exec(ctx, `
		INSERT INTO template_versions (template_id, seq, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, author_sub, author_name, base_version_id)
		SELECT id, 3, 'YARIM TASLAK', '<p>YARIM</p>', 'html', '<p>YARIM</p>', 'YARIM', 'operator', $2, 'Başka Operatör',
		       published_version_id
		FROM templates
		WHERE id = $1`, seeded.ID, operatorSub); err != nil {
		t.Fatal(err)
	}

	response, body = sendJSON(t, app, fiber.MethodPost, "/mail_tasks/single", map[string]any{
		"template_key": key, "recipient_email": "uye@yildizskylab.com", "recipient_full_name": "Deniz Kaya",
		"body_variables": map[string]string{},
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("send = %d %s", response.StatusCode, body)
	}

	var subject, text, html string
	if err := db.Conn.QueryRow(ctx, `SELECT subject, body, body_html FROM mail_queue WHERE recipient_email = 'uye@yildizskylab.com'`).
		Scan(&subject, &text, &html); err != nil {
		t.Fatal(err)
	}
	if subject != "Aramıza hoş geldin, Deniz Kaya" || text != "Aramıza hoş geldin Deniz Kaya" || html != "<p>Aramıza hoş geldin Deniz Kaya</p>" {
		t.Fatalf("queued %q / %q / %q, want the published edit rendered for the recipient", subject, text, html)
	}
}

// servedVersion is a version as the version routes serve it.
type servedVersion struct {
	ID               uuid.UUID `json:"id"`
	TemplateID       uuid.UUID `json:"template_id"`
	Seq              int       `json:"seq"`
	Subject          string    `json:"subject"`
	RequestedSubject *string   `json:"requested_subject"`
	MainMode         string    `json:"main_mode"`
	Author           struct {
		Kind string  `json:"kind"`
		Sub  *string `json:"sub"`
		Name *string `json:"name"`
	} `json:"author"`
	CreatedAt     time.Time  `json:"created_at"`
	PublishedAt   *time.Time `json:"published_at"`
	BaseVersionID *uuid.UUID `json:"base_version_id"`
	Current       bool       `json:"current"`
}

// A template with a history of three: the seed's, an operator's edit in the
// old panel, and a draft another operator has not published.
func templateWithHistory(t *testing.T, db *database.Store, app *fiber.App) (database.Template, []storedVersion) {
	t.Helper()
	const key = "core.certificate"
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Sertifika", "subject": "Katılım sertifikan hazır",
		"html_content": `<a href="{{.VerifyURL}}">Doğrula</a>`, "plain_text_content": "Doğrula: {{.VerifyURL}}",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var template database.Template
	if err := json.Unmarshal(body, &template); err != nil {
		t.Fatal(err)
	}
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+template.ID.String(), map[string]any{
		"name": template.Name, "subject": "Sertifikan hazır, {{.FullName}}", "key": key,
		"html_content": `<p>Tebrikler</p><a href="{{.VerifyURL}}">Doğrula</a>`, "plain_text_content": "Tebrikler. Doğrula: {{.VerifyURL}}",
		"react_email_content": panelSource,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("edit = %d %s", response.StatusCode, body)
	}
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO template_versions (template_id, seq, subject, jsx_source, visual_source, main_mode, html_content,
		                               plain_text_content, author_kind, author_sub, author_name, base_version_id)
		SELECT id, 3, 'Taslak konu', $2, '{"type": "doc", "content": []}', 'visual', '<p>Taslak</p>', 'Taslak',
		       'operator', 'b7d1e7a2-3c1f-4c55-9d6e-0a1b2c3d4e5f', 'Can Demir', published_version_id
		FROM templates
		WHERE id = $1`, template.ID, panelSource); err != nil {
		t.Fatal(err)
	}
	versions, _ := storedVersions(t, db, template.ID)
	return template, versions
}

// A template's history, newest first: who wrote each version (an operator by
// name, or a Template seed), when, whether it is published or a draft, which
// Authoring mode is its Main source, what it started from, and which one the
// template sends. No sources or render: those are one version's to read.
func TestListTemplateVersionsNewestFirst(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	template, stored := templateWithHistory(t, db, app)

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+template.ID.String()+"/versions", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "3" {
		t.Fatalf("GET versions = %d total %q %s", response.StatusCode, response.Header.Get("X-Total-Count"), body)
	}

	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		for _, big := range []string{"jsx_source", "visual_source", "html_source", "html_content", "plain_text_content"} {
			if _, ok := row[big]; ok {
				t.Errorf("the list carries %s; it belongs to reading one version", big)
			}
		}
	}

	var versions []servedVersion
	if err := json.Unmarshal(body, &versions); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[0].Seq != 3 || versions[1].Seq != 2 || versions[2].Seq != 1 {
		t.Fatalf("versions = %+v, want 3, 2, 1", versions)
	}
	draft, edit, seed := versions[0], versions[1], versions[2]

	if draft.ID != stored[2].ID || draft.TemplateID != template.ID || draft.PublishedAt != nil || draft.Current ||
		draft.MainMode != "visual" || draft.Subject != "Taslak konu" ||
		draft.Author.Kind != "operator" || !sameString(draft.Author.Name, "Can Demir") ||
		draft.BaseVersionID == nil || *draft.BaseVersionID != edit.ID || !draft.CreatedAt.Equal(stored[2].CreatedAt) {
		t.Errorf("draft = %+v, want Can Demir's unpublished Visual draft started from the edit", draft)
	}
	if edit.PublishedAt == nil || !edit.Current || edit.MainMode != "jsx" || edit.Subject != "Sertifikan hazır, {{.FullName}}" ||
		edit.Author.Kind != "operator" || !sameString(edit.Author.Name, operatorName) || !sameString(edit.Author.Sub, operatorSub) ||
		edit.BaseVersionID == nil || *edit.BaseVersionID != seed.ID {
		t.Errorf("edit = %+v, want the operator's published JSX version, the one being sent", edit)
	}
	if seed.PublishedAt == nil || seed.Current || seed.MainMode != "html" || seed.Author.Kind != "template_seed" || seed.BaseVersionID != nil ||
		!sameString(seed.RequestedSubject, "Katılım sertifikan hazır") || edit.RequestedSubject != nil {
		t.Errorf("seed = %+v, want the Template seed's published HTML version, no longer sent", seed)
	}

	// Pages as the other lists do.
	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+template.ID.String()+"/versions?_start=1&_end=2", nil))
	if err != nil {
		t.Fatal(err)
	}
	var page []servedVersion
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Seq != 2 || response.Header.Get("X-Total-Count") != "3" {
		t.Fatalf("page = %+v total %q, want version 2 of 3", page, response.Header.Get("X-Total-Count"))
	}
}

func TestListTemplateVersionsOfAnArchivedOrUnknownTemplate(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	archived := createTemplate(t, app, map[string]any{
		"name": "Eski", "subject": "Eski", "html_content": "<p>Eski</p>", "plain_text_content": "Eski",
		"react_email_content": panelSource,
	})
	if response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+archived.ID.String(), nil)); err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("archive: %v %v", response, err)
	}

	// History outlives archiving: an archived template's versions stay
	// readable, so it can be looked at before it is restored.
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+archived.ID.String()+"/versions", nil))
	if err != nil {
		t.Fatal(err)
	}
	var versions []servedVersion
	if err := json.NewDecoder(response.Body).Decode(&versions); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK || len(versions) != 1 || !versions[0].Current {
		t.Fatalf("archived template's versions = %d %+v", response.StatusCode, versions)
	}

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+id+"/versions", nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNotFound {
			t.Errorf("versions of template %q = %d, want 404", id, response.StatusCode)
		}
	}
}

// Reading one version gives it whole: every source it holds, null for the
// Authoring modes it has none in, and the Main source's render.
func TestReadOneTemplateVersion(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	template, stored := templateWithHistory(t, db, app)

	read := func(templateID uuid.UUID, versionID string) (*http.Response, map[string]any) {
		t.Helper()
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+templateID.String()+"/versions/"+versionID, nil))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		var object map[string]any
		if response.StatusCode == fiber.StatusOK {
			if err := json.Unmarshal(body, &object); err != nil {
				t.Fatalf("%s: %v", body, err)
			}
		}
		return response, object
	}

	response, seed := read(template.ID, stored[0].ID.String())
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET seed version = %d", response.StatusCode)
	}
	author, _ := seed["author"].(map[string]any)
	if seed["seq"] != float64(1) || seed["main_mode"] != "html" || seed["current"] != false || author["kind"] != "template_seed" ||
		seed["jsx_source"] != nil || seed["visual_source"] != nil || seed["html_source"] != `<a href="{{.VerifyURL}}">Doğrula</a>` ||
		seed["html_content"] != `<a href="{{.VerifyURL}}">Doğrula</a>` || seed["plain_text_content"] != "Doğrula: {{.VerifyURL}}" ||
		seed["subject"] != "Katılım sertifikan hazır" || seed["requested_subject"] != "Katılım sertifikan hazır" ||
		seed["published_at"] == nil || seed["base_version_id"] != nil {
		t.Errorf("seed version = %v", seed)
	}

	response, edit := read(template.ID, stored[1].ID.String())
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET edit version = %d", response.StatusCode)
	}
	// The operator's JSX became the Main source; the seed's HTML source is
	// carried along beside it.
	if edit["main_mode"] != "jsx" || edit["jsx_source"] != panelSource || edit["html_source"] != `<a href="{{.VerifyURL}}">Doğrula</a>` || edit["visual_source"] != nil ||
		edit["current"] != true || edit["html_content"] != `<p>Tebrikler</p><a href="{{.VerifyURL}}">Doğrula</a>` ||
		edit["base_version_id"] != stored[0].ID.String() {
		t.Errorf("edit version = %v", edit)
	}

	response, draft := read(template.ID, stored[2].ID.String())
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET draft = %d", response.StatusCode)
	}
	visual, _ := draft["visual_source"].(map[string]any)
	if draft["main_mode"] != "visual" || visual["type"] != "doc" || draft["jsx_source"] != panelSource ||
		draft["published_at"] != nil || draft["current"] != false {
		t.Errorf("draft = %v, want the Visual document as an object beside its JSX source, unpublished", draft)
	}

	// A version is read through its own template only.
	other := createTemplate(t, app, map[string]any{
		"name": "Başka", "subject": "Başka", "html_content": "<p>Başka</p>", "plain_text_content": "Başka",
		"react_email_content": panelSource,
	})
	for name, path := range map[string][2]string{
		"another template's version": {other.ID.String(), stored[1].ID.String()},
		"an unknown version":         {template.ID.String(), uuid.NewString()},
		"a malformed version id":     {template.ID.String(), "not-a-uuid"},
		"a malformed template id":    {"not-a-uuid", stored[1].ID.String()},
	} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+path[0]+"/versions/"+path[1], nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, response.StatusCode)
		}
	}
}

func TestTemplateVersionResponsesAreServedAsDocumented(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	template, stored := templateWithHistory(t, db, app)

	for _, tc := range []struct{ documented, served string }{
		{"/templates/{id}/versions", "/templates/" + template.ID.String() + "/versions"},
		{"/templates/{id}/versions/{versionId}", "/templates/" + template.ID.String() + "/versions/" + stored[1].ID.String()},
		// The template itself keeps its documented shape.
		{"/templates/{id}", "/templates/" + template.ID.String()},
	} {
		documented, served := documentedFields(t, tc.documented), servedFields(t, app, tc.served)
		if fmt.Sprint(documented) != fmt.Sprint(served) {
			t.Errorf("GET %s documents %v but serves %v", tc.documented, documented, served)
		}
	}
	if documented := documentedFields(t, "/templates/{id}"); fmt.Sprint(documented) != fmt.Sprint(templateFields) {
		t.Errorf("GET /templates/{id} documents %v, want %v", documented, templateFields)
	}
}

// A write that leaves the template as its published version already is —
// subject, every source, Main source, render — is not a change and records no
// version: a routine seed must not make every open draft stale (ticket 07), nor
// fill the history with copies. name is not part of a version.
func TestUnchangedWritesRecordNoVersion(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	const key = "keycloak.reset-password"
	seed := map[string]any{
		"name": "Parola Sıfırlama", "subject": "SKY LAB parola sıfırlama isteği",
		"html_content": `<a href="{{.link}}">Sıfırla</a>`, "plain_text_content": "Sıfırla: {{.link}}",
		"react_email_content": seedPointerComment(key), "system": true,
	}
	var seeded database.Template
	for run := 0; run < 3; run++ {
		response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, seed)
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("seed run %d = %d %s", run+1, response.StatusCode, body)
		}
		assertTemplateShape(t, body)
		if err := json.Unmarshal(body, &seeded); err != nil {
			t.Fatal(err)
		}
	}
	versions, published := storedVersions(t, db, seeded.ID)
	if len(versions) != 1 || seeded.PublishedVersionID == nil || *seeded.PublishedVersionID != versions[0].ID || *published != versions[0].ID {
		t.Fatalf("three identical seeds left %d versions (row copy of %v, answered %v), want 1", len(versions), published, seeded.PublishedVersionID)
	}

	for name, change := range map[string]map[string]any{
		"an identical save": {},
		"a rename":          {"name": "Parola sıfırlama (Keycloak)"},
	} {
		body := map[string]any{
			"name": seeded.Name, "subject": seeded.Subject, "key": key,
			"html_content": seeded.HtmlContent, "plain_text_content": seeded.PlainTextContent,
			"react_email_content": seeded.ReactEmailContent,
		}
		for field, value := range change {
			body[field] = value
		}
		response, raw := sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), body)
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("%s = %d %s", name, response.StatusCode, raw)
		}
		assertTemplateShape(t, raw)
		if versions, _ := storedVersions(t, db, seeded.ID); len(versions) != 1 {
			t.Fatalf("%s recorded a version; versions = %d, want 1", name, len(versions))
		}
	}
}

// publishVisualVersion saves and publishes a draft holding a Visual source as
// the Main source and a JSX source beside it.
func publishVisualVersion(t *testing.T, app *fiber.App, db *database.Store, templateID uuid.UUID) storedVersion {
	t.Helper()
	var template database.Template
	if err := db.Conn.QueryRow(context.Background(), `SELECT published_version_id FROM templates WHERE id = $1`, templateID).
		Scan(&template.PublishedVersionID); err != nil {
		t.Fatal(err)
	}
	response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+templateID.String()+"/drafts", map[string]any{
		"subject": "Görsel konu", "main_mode": "visual", "jsx_source": panelSource,
		"visual_source": json.RawMessage(`{"type": "doc", "content": [{"type": "paragraph"}]}`),
		"html_content":  "<p>Görsel</p>", "plain_text_content": "Görsel", "base_version_id": template.PublishedVersionID,
	})
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("draft = %d %s", response.StatusCode, body)
	}
	var draft struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &draft); err != nil {
		t.Fatal(err)
	}
	if response, body := sendJSON(t, app, fiber.MethodPost, "/templates/"+templateID.String()+"/versions/"+draft.ID.String()+"/publish", nil); response.StatusCode != fiber.StatusOK {
		t.Fatalf("publish = %d %s", response.StatusCode, body)
	}
	versions, _ := storedVersions(t, db, templateID)
	return versions[len(versions)-1]
}

// The old panel and the seed write only a subject, a body and JSX. Every other
// source the published version holds is carried into the version they write.
func TestLegacyWritesCarryTheOtherSourcesForward(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	const key = "core.welcome"
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Hoş Geldin", "subject": "Hoş geldin",
		"html_content": "<p>Hoş geldin</p>", "plain_text_content": "Hoş geldin",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	var template database.Template
	if err := json.Unmarshal(body, &template); err != nil {
		t.Fatal(err)
	}
	visual := publishVisualVersion(t, app, db, template.ID)
	if err := db.Conn.QueryRow(context.Background(), `SELECT react_email_content, html_content, plain_text_content FROM templates WHERE id = $1`, template.ID).
		Scan(&template.ReactEmailContent, &template.HtmlContent, &template.PlainTextContent); err != nil {
		t.Fatal(err)
	}
	patch := func(subject, html, plainText, react string) storedVersion {
		t.Helper()
		response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+template.ID.String(), map[string]any{
			"name": template.Name, "subject": subject, "key": key,
			"html_content": html, "plain_text_content": plainText, "react_email_content": react,
		})
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("PATCH = %d %s", response.StatusCode, body)
		}
		versions, _ := storedVersions(t, db, template.ID)
		return versions[len(versions)-1]
	}

	// Publishing a Visual Main source leaves the old panel no JSX to edit, so
	// it cannot save the template around it: a save with no JSX is refused
	// and records nothing.
	if template.ReactEmailContent != "" {
		t.Fatalf("react_email_content = %q, want nothing for a Visual Main source", template.ReactEmailContent)
	}
	if response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+template.ID.String(), map[string]any{
		"name": template.Name, "subject": "Aramıza hoş geldin", "key": key, "html_content": template.HtmlContent,
		"plain_text_content": template.PlainTextContent, "react_email_content": template.ReactEmailContent,
	}); response.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("an old panel save with no JSX = %d %s, want 400", response.StatusCode, body)
	}
	if versions, _ := storedVersions(t, db, template.ID); versions[len(versions)-1].ID != visual.ID {
		t.Fatalf("a refused save recorded a version")
	}

	// Writing JSX in the old panel makes JSX the Main source; the Visual source
	// and the HTML source the seed left are kept.
	edited := panelSource + "\n// düzenlendi\n"
	jsx := patch("Aramıza hoş geldin", "<p>JSX</p>", "JSX", edited)
	if jsx.MainMode != "jsx" || !sameString(jsx.JSXSource, edited) || string(jsx.VisualSource) != string(visual.VisualSource) ||
		visual.HTMLSource == nil || !sameString(jsx.HTMLSource, *visual.HTMLSource) || jsx.HTMLContent != "<p>JSX</p>" {
		t.Fatalf("JSX edit = %+v, want JSX as the Main source with the Visual and HTML sources kept", jsx)
	}

	// A seed with a new body and no JSX makes that body the HTML Main source;
	// the Visual and JSX sources are kept.
	response, body = sendJSON(t, app, fiber.MethodPut, "/templates/by-key/"+key, map[string]any{
		"name": "Hoş Geldin", "subject": "Hoş geldin",
		"html_content": "<p>Repo gövdesi</p>", "plain_text_content": "Repo gövdesi",
		"react_email_content": seedPointerComment(key), "system": true,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("reseed = %d %s", response.StatusCode, body)
	}
	versions, _ := storedVersions(t, db, template.ID)
	seeded := versions[len(versions)-1]
	if seeded.AuthorKind != "template_seed" || seeded.MainMode != "html" || !sameString(seeded.HTMLSource, "<p>Repo gövdesi</p>") ||
		string(seeded.VisualSource) != string(visual.VisualSource) || !sameString(seeded.JSXSource, edited) {
		t.Fatalf("seed = %+v, want the repo body as the HTML Main source, Visual and JSX sources kept", seeded)
	}
}

// Between the migration and the new binary taking over, the old binary still
// writes rows without versions. The next write versions the row as it now is.
func TestAWriteVersionsARowWrittenWithoutOne(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	created := createTemplate(t, app, map[string]any{
		"name": "Bülten", "subject": "Bülten", "html_content": "<p>Bülten</p>", "plain_text_content": "Bülten",
		"react_email_content": panelSource,
	})
	// The old binary's edit: the row changes, no version is written.
	drifted := panelSource + "\n// eski sürümle kaydedildi\n"
	if _, err := db.Conn.Exec(context.Background(), `
		UPDATE templates SET subject = 'Eylül bülteni', html_content = '<p>Eylül</p>', plain_text_content = 'Eylül',
		                     react_email_content = $2, updated_at = NOW()
		WHERE id = $1`, created.ID, drifted); err != nil {
		t.Fatal(err)
	}

	// The new binary's next write only renames it.
	response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+created.ID.String(), map[string]any{
		"name": "Eylül bülteni", "subject": "Eylül bülteni", "html_content": "<p>Eylül</p>", "plain_text_content": "Eylül",
		"react_email_content": drifted,
	})
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH = %d %s", response.StatusCode, body)
	}
	versions, published := storedVersions(t, db, created.ID)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want the row's drift recorded as a second", len(versions))
	}
	latest := versions[1]
	if latest.Subject != "Eylül bülteni" || latest.HTMLContent != "<p>Eylül</p>" || !sameString(latest.JSXSource, drifted) ||
		published == nil || *published != latest.ID {
		t.Fatalf("version = %+v, want the row as it now is", latest)
	}
}
