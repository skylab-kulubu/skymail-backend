package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

type lifecycleKeycloakStub struct{}

type failingLifecycleKeycloakStub struct{}

func (failingLifecycleKeycloakStub) ListGroups(context.Context) ([]*gocloak.Group, error) {
	return nil, errors.New("unexpected Keycloak list lookup")
}

func (failingLifecycleKeycloakStub) GetGroup(context.Context, string) (*gocloak.Group, error) {
	return nil, errors.New("unexpected Keycloak group lookup")
}

func (failingLifecycleKeycloakStub) GetGroupMembers(context.Context, string) ([]*gocloak.User, error) {
	return nil, errors.New("unexpected Keycloak member lookup")
}

type lifecycleMailerStub struct {
	enqueueCalls int
}

func (*lifecycleMailerStub) Start(context.Context, int) {}

func (m *lifecycleMailerStub) Enqueue(context.Context, database.CreateMailTaskParams) (uuid.UUID, error) {
	m.enqueueCalls++
	return uuid.New(), nil
}

func (*lifecycleMailerStub) EnqueueSingle(context.Context, database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (*lifecycleMailerStub) EnqueueWithRecipients(context.Context, mailer.EnqueueWithRecipientsParams) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (lifecycleKeycloakStub) ListGroups(context.Context) ([]*gocloak.Group, error) {
	return []*gocloak.Group{}, nil
}

func (lifecycleKeycloakStub) GetGroup(context.Context, string) (*gocloak.Group, error) {
	return nil, nil
}

func (lifecycleKeycloakStub) GetGroupMembers(context.Context, string) ([]*gocloak.User, error) {
	return []*gocloak.User{}, nil
}

func lifecycleHandlerStore(t *testing.T) *database.Store {
	t.Helper()
	pool := testpostgres.Start(t)
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate lifecycle handler test")
	}
	migrationsDir := filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations")
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(migration)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
	return database.NewStore(pool)
}

func lifecycleTestApp(t *testing.T, db *database.Store) *fiber.App {
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
	app.Get("/templates", templates.GetTemplates)
	app.Get("/templates/:id", templates.GetTemplate)
	app.Delete("/templates/:id", templates.DeleteTemplate)
	app.Post("/templates/:id/restore", templates.RestoreTemplate)

	lists := NewListHandler(db, lifecycleKeycloakStub{})
	app.Get("/mailing_lists", lists.GetLists)
	app.Get("/mailing_lists/:id", lists.GetList)
	app.Delete("/mailing_lists/:id", lists.DeleteList)
	app.Post("/mailing_lists/:id/restore", lists.RestoreList)
	return app
}

func TestTemplateHTTPArchiveLifecycle(t *testing.T) {
	db := lifecycleHandlerStore(t)
	template, err := db.CreateTemplate(context.Background(), database.CreateTemplateParams{
		Name: "Bülten", Subject: "SKY LAB", HtmlContent: "<p>SKY LAB</p>",
		PlainTextContent: "SKY LAB", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	app := lifecycleTestApp(t, db)

	for attempt := 0; attempt < 2; attempt++ {
		response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/templates/"+template.ID.String(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNoContent {
			t.Fatalf("DELETE attempt %d status = %d, want 204", attempt+1, response.StatusCode)
		}
	}

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+template.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNotFound {
		t.Fatalf("archived template GET status = %d, want 404", response.StatusCode)
	}
	for _, path := range []string{"/templates", "/templates?lifecycle=all"} {
		response, err = app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		wantCount := "0"
		if path == "/templates?lifecycle=all" {
			wantCount = "1"
		}
		if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != wantCount {
			t.Fatalf("GET %s status=%d count=%q, want %s", path, response.StatusCode, response.Header.Get("X-Total-Count"), wantCount)
		}
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/templates?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "1" {
		t.Fatalf("inactive templates status=%d count=%q", response.StatusCode, response.Header.Get("X-Total-Count"))
	}
	var archived []database.Template
	if err := json.NewDecoder(response.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != "31ef736f-72da-4a40-8791-d523199cf9f0" {
		t.Fatalf("inactive templates = %+v", archived)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response, err = app.Test(httptest.NewRequest(fiber.MethodPost, "/templates/"+template.ID.String()+"/restore", nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("restore attempt %d status = %d, want 200", attempt+1, response.StatusCode)
		}
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/templates/"+template.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("restored template GET status = %d, want 200", response.StatusCode)
	}
}

func TestMailingListHTTPArchiveLifecycle(t *testing.T) {
	db := lifecycleHandlerStore(t)
	list, err := db.CreateMailingList(context.Background(), "WebLab")
	if err != nil {
		t.Fatal(err)
	}
	app := lifecycleTestApp(t, db)

	for attempt := 0; attempt < 2; attempt++ {
		response, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/mailing_lists/"+list.ID.String(), nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNoContent {
			t.Fatalf("DELETE attempt %d status = %d, want 204", attempt+1, response.StatusCode)
		}
	}
	for _, path := range []string{"/mailing_lists", "/mailing_lists?lifecycle=all"} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		wantCount := "0"
		if path == "/mailing_lists?lifecycle=all" {
			wantCount = "1"
		}
		if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != wantCount {
			t.Fatalf("GET %s status=%d count=%q, want %s", path, response.StatusCode, response.Header.Get("X-Total-Count"), wantCount)
		}
	}

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/mailing_lists?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "1" {
		t.Fatalf("inactive mailing lists status=%d count=%q", response.StatusCode, response.Header.Get("X-Total-Count"))
	}
	var archived []MailingListItem
	if err := json.NewDecoder(response.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ArchivedAt == nil || archived[0].ArchivedBy == nil || *archived[0].ArchivedBy != "31ef736f-72da-4a40-8791-d523199cf9f0" {
		t.Fatalf("inactive mailing lists = %+v", archived)
	}

	for attempt := 0; attempt < 2; attempt++ {
		response, err = app.Test(httptest.NewRequest(fiber.MethodPost, "/mailing_lists/"+list.ID.String()+"/restore", nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusOK {
			t.Fatalf("restore attempt %d status = %d, want 200", attempt+1, response.StatusCode)
		}
	}

	response, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/mailing_lists/"+list.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("restored mailing list GET status = %d, want 200", response.StatusCode)
	}
}

func TestLifecycleFilterRejectsUnknownValue(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := lifecycleTestApp(t, db)
	for _, path := range []string{"/templates?lifecycle=deleted", "/mailing_lists?lifecycle=deleted"} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("GET %s status = %d, want 400", path, response.StatusCode)
		}
	}
}

func TestCreateTaskRejectsArchivedTemplateBeforeEnqueue(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	actor := "31ef736f-72da-4a40-8791-d523199cf9f0"
	template, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Arşiv", Subject: "Konu", HtmlContent: "<p>İçerik</p>",
		PlainTextContent: "İçerik", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "Güncel liste")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveTemplate(ctx, database.ArchiveTemplateParams{ID: template.ID, ArchivedBy: &actor}); err != nil {
		t.Fatal(err)
	}

	mailerStub := &lifecycleMailerStub{}
	handler := NewMailHandler(db, mailerStub, lifecycleKeycloakStub{})
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}})
	app.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", actor)
		return c.Next()
	})
	app.Post("/mail_tasks", handler.CreateTask)

	body := []byte(`{"template_id":"` + template.ID.String() + `","mail_list_id":"` + list.ID.String() + `","body_variables":{}}`)
	request := httptest.NewRequest(fiber.MethodPost, "/mail_tasks", bytes.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNotFound {
		t.Fatalf("archived-template task status = %d, want 404", response.StatusCode)
	}
	if mailerStub.enqueueCalls != 0 {
		t.Fatalf("mailer enqueue calls = %d, want 0", mailerStub.enqueueCalls)
	}
}

func TestArchivedInternalListDoesNotFallThroughToKeycloak(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	actor := "31ef736f-72da-4a40-8791-d523199cf9f0"
	template, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Güncel", Subject: "Konu", HtmlContent: "<p>İçerik</p>",
		PlainTextContent: "İçerik", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "Arşiv liste")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveMailingList(ctx, database.ArchiveMailingListParams{ID: list.ID, ArchivedBy: &actor}); err != nil {
		t.Fatal(err)
	}

	mailerStub := &lifecycleMailerStub{}
	listHandler := NewListHandler(db, failingLifecycleKeycloakStub{})
	mailHandler := NewMailHandler(db, mailerStub, failingLifecycleKeycloakStub{})
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}})
	app.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", actor)
		return c.Next()
	})
	app.Get("/mailing_lists/:id", listHandler.GetList)
	app.Get("/mailing_lists/:id/recipients", listHandler.GetRecipients)
	app.Post("/mail_tasks", mailHandler.CreateTask)

	for _, path := range []string{
		"/mailing_lists/" + list.ID.String(),
		"/mailing_lists/" + list.ID.String() + "/recipients",
	} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, response.StatusCode)
		}
	}

	body := []byte(`{"template_id":"` + template.ID.String() + `","mail_list_id":"` + list.ID.String() + `","body_variables":{}}`)
	request := httptest.NewRequest(fiber.MethodPost, "/mail_tasks", bytes.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNotFound {
		t.Fatalf("archived-list task status = %d, want 404", response.StatusCode)
	}
	if mailerStub.enqueueCalls != 0 {
		t.Fatalf("mailer enqueue calls = %d, want 0", mailerStub.enqueueCalls)
	}
}
