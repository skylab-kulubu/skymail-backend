package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// recordingMailerStub remembers which template a send resolved to, which is the
// whole point of addressing a template by key.
type recordingMailerStub struct {
	singleTemplateID uuid.UUID
}

func (*recordingMailerStub) Start(context.Context, int) {}

func (*recordingMailerStub) Enqueue(context.Context, database.CreateMailTaskParams) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (m *recordingMailerStub) EnqueueSingle(_ context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	if arg.TemplateID != nil {
		m.singleTemplateID = *arg.TemplateID
	}
	return uuid.New(), nil
}

func (*recordingMailerStub) EnqueueWithRecipients(context.Context, mailer.EnqueueWithRecipientsParams) (uuid.UUID, error) {
	return uuid.New(), nil
}

func systemTemplateApp(t *testing.T, db *database.Store, mail mailer.Mailer) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		var appErr *apperrors.AppError
		if errors.As(err, &appErr) {
			return c.Status(appErr.Status).JSON(appErr)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}})
	app.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", "31ef736f-72da-4a40-8791-d523199cf9f0")
		return c.Next()
	})

	templates := NewTemplateHandler(db)
	app.Get("/templates/:id", templates.GetTemplate)
	app.Patch("/templates/:id", templates.UpdateTemplate)
	app.Delete("/templates/:id", templates.DeleteTemplate)
	app.Get("/templates/by-key/:key", templates.GetTemplateByKey)
	app.Put("/templates/by-key/:key", templates.UpsertTemplateByKey)

	mails := NewMailHandler(db, mail, lifecycleKeycloakStub{})
	app.Post("/mail_tasks/single", mails.SendSingle)

	return app
}

func seedSystemTemplate(t *testing.T, db *database.Store, key string) database.Template {
	t.Helper()
	template, err := db.UpsertTemplateByKey(context.Background(), database.UpsertTemplateByKeyParams{
		Key:               key,
		Name:              "Parola Sıfırlama",
		Subject:           "SKY LAB parola sıfırlama isteği",
		HtmlContent:       "<p>{{.link}}</p>",
		PlainTextContent:  "{{.link}}",
		ReactEmailContent: "{}",
		System:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func TestSystemTemplateCannotBeArchived(t *testing.T) {
	db := lifecycleHandlerStore(t)
	template := seedSystemTemplate(t, db, "keycloak.reset-password")
	app := systemTemplateApp(t, db, &recordingMailerStub{})

	response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+template.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("DELETE status = %d, want 409", response.StatusCode)
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+template.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("template GET after refused archive = %d, want 200", response.StatusCode)
	}
}

func TestSystemTemplateKeyCannotBeRenamed(t *testing.T) {
	db := lifecycleHandlerStore(t)
	template := seedSystemTemplate(t, db, "keycloak.verify-email")
	app := systemTemplateApp(t, db, &recordingMailerStub{})

	renamed, _ := json.Marshal(map[string]any{
		"name": "Yeni ad", "subject": "Yeni konu",
		"html_content": "<p>x</p>", "plain_text_content": "x", "react_email_content": "{}",
		"key": "keycloak.something-else",
	})
	request := httptest.NewRequest(fiber.MethodPatch, "/templates/"+template.ID.String(), bytes.NewReader(renamed))
	request.Header.Set("Content-Type", "application/json")

	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusConflict {
		t.Fatalf("PATCH status = %d, want 409", response.StatusCode)
	}

	// The wording is still editable, only the key is pinned.
	reworded, _ := json.Marshal(map[string]any{
		"name": "Parola Sıfırlama", "subject": "Yeni konu",
		"html_content": "<p>x</p>", "plain_text_content": "x", "react_email_content": "{}",
		"key": "keycloak.verify-email",
	})
	request = httptest.NewRequest(fiber.MethodPatch, "/templates/"+template.ID.String(), bytes.NewReader(reworded))
	request.Header.Set("Content-Type", "application/json")

	response, err = app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("rewording a system template = %d, want 200", response.StatusCode)
	}
}

func TestSingleSendResolvesTemplateByKey(t *testing.T) {
	db := lifecycleHandlerStore(t)
	template := seedSystemTemplate(t, db, "keycloak.reset-password")
	stub := &recordingMailerStub{}
	app := systemTemplateApp(t, db, stub)

	body, _ := json.Marshal(map[string]any{
		"template_key":        "keycloak.reset-password",
		"recipient_email":     "uye@yildizskylab.com",
		"recipient_full_name": "SKY LAB Üyesi",
		"body_variables":      map[string]string{"link": "https://my.yildizskylab.com/x"},
	})
	request := httptest.NewRequest(fiber.MethodPost, "/mail_tasks/single", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusCreated {
		t.Fatalf("single send status = %d, want 201", response.StatusCode)
	}
	if stub.singleTemplateID != template.ID {
		t.Fatalf("resolved template = %s, want %s", stub.singleTemplateID, template.ID)
	}
}

func TestSingleSendRejectsMissingTemplateHandle(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := systemTemplateApp(t, db, &recordingMailerStub{})

	body, _ := json.Marshal(map[string]any{
		"recipient_email":     "uye@yildizskylab.com",
		"recipient_full_name": "SKY LAB Üyesi",
	})
	request := httptest.NewRequest(fiber.MethodPost, "/mail_tasks/single", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

// Seeding is how templates get their content, so a reseed must be able to run
// repeatedly and must bring an archived template back rather than duplicating it.
func TestUpsertByKeyIsIdempotentAndUnarchives(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := systemTemplateApp(t, db, &recordingMailerStub{})

	payload, _ := json.Marshal(map[string]any{
		"name": "Hoş Geldin", "subject": "SKY LAB'e hoş geldin",
		"html_content": "<p>v1</p>", "plain_text_content": "v1", "react_email_content": "{}",
	})
	put := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(fiber.MethodPut, "/templates/by-key/core.welcome", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("PUT status = %d, want 200", response.StatusCode)
		}
		recorder := httptest.NewRecorder()
		_, _ = recorder.Body.ReadFrom(response.Body)
		return recorder
	}

	first := put()
	var created database.Template
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	second := put()
	var reseeded database.Template
	if err := json.Unmarshal(second.Body.Bytes(), &reseeded); err != nil {
		t.Fatal(err)
	}
	if reseeded.ID != created.ID {
		t.Fatalf("reseed created a second template (%s then %s)", created.ID, reseeded.ID)
	}

	if _, err := db.ArchiveTemplate(context.Background(), database.ArchiveTemplateParams{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	put()

	if _, err := db.GetTemplateById(context.Background(), created.ID); err != nil {
		t.Fatalf("reseed left the template archived: %v", err)
	}
}

// core sends welcome and certificate mail by uuid and never looks at the
// response — internal/mail/mail.go and certificate.go both discard it — so if
// the template that SKYMAIL_WELCOME_TEMPLATE_ID or SKYMAIL_CERTIFICATE_TEMPLATE_ID
// points at is archived, the mail stops with nothing to see. This pins what the
// real mailer does in that case: it refuses and writes no task.
func TestEnqueueSingleRefusesArchivedTemplate(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()

	template, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Katılım Sertifikası", Subject: "Katılım sertifikan hazır",
		HtmlContent: "<p>{{.EventName}}</p>", PlainTextContent: "{{.EventName}}", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveTemplate(ctx, database.ArchiveTemplateParams{ID: template.ID}); err != nil {
		t.Fatal(err)
	}

	realMailer := mailer.NewMailer(db, mailer.SMTPConfig{})
	taskID, err := realMailer.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy:            "31ef736f-72da-4a40-8791-d523199cf9f0",
		TemplateID:        &template.ID,
		BodyVariables:     []byte(`{"EventName":"GECEKODU 2026"}`),
		RecipientFullName: "SKY LAB Üyesi",
		RecipientEmail:    "uye@yildizskylab.com",
	})
	if err == nil {
		t.Fatalf("archived template enqueued a send (task %s)", taskID)
	}

	var tasks int64
	tasks, err = db.CountMailTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("mail_tasks = %d, want 0", tasks)
	}
}
