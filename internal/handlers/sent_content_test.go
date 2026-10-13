package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

const (
	contentSender = "31ef736f-72da-4a40-8791-d523199cf9f0"
	contentOther  = "6b0f1d1e-3c55-4c1e-9d0c-2f6f3b7c9a11"
)

// contentApp serves the send routes with the caller's subject and roles taken
// from the request, as the auth middleware leaves them.
func contentApp(t *testing.T, db *database.Store, now time.Time) *fiber.App {
	t.Helper()
	handler := &mailHandlerImpl{db: db, mailer: &lifecycleMailerStub{}, kc: lifecycleKeycloakStub{}, now: func() time.Time { return now },
		contentRetentionDays: 90}
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
		c.Locals("user_id", c.Get("X-Test-Sub"))
		if name := c.Get("X-Test-Name"); name != "" {
			c.Locals("user_name", name)
		}
		c.Locals("roles", strings.Split(c.Get("X-Test-Roles"), ","))
		return c.Next()
	})
	app.Get("/mail_tasks", handler.GetTasks)
	app.Get("/mail_tasks/:id", handler.GetTask)
	app.Get("/mail_tasks/:id/queue/:itemId/content", handler.GetQueueItemContent)
	return app
}

type contentSend struct {
	sensitive *bool
	list      bool
	sentBy    string
	createdAt time.Time
	cleared   bool
}

// seedContentSend writes a send of one recipient whose rendered mail names a
// code, and returns the send and its queue row.
func seedContentSend(t *testing.T, db *database.Store, send contentSend) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var templateID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('Şablon', 'Konu', '<p>x</p>', 'x', '', $1) RETURNING id`, send.sensitive).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	var list *uuid.UUID
	if send.list {
		id := uuid.New()
		list = &id
	}
	var cleared *time.Time
	if send.cleared {
		cleared = &send.createdAt
	}
	var taskID, rowID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, created_at, content_cleared_at)
		VALUES ($1, $2, $3, '{"Heading":"Merhaba"}', $4, $5) RETURNING id`,
		send.sentBy, templateID, list, send.createdAt, cleared).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	subject, body, html := "Duyuru", "Merhaba Ayşe", "<p>Merhaba <b>Ayşe</b></p>"
	if send.cleared {
		subject, body, html = "", "", ""
	}
	var htmlValue *string
	if html != "" {
		htmlValue = &html
	}
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
		VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', $2, $3, $4, 'sent', $5) RETURNING id`,
		taskID, subject, body, htmlValue, send.createdAt).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	return taskID, rowID
}

type contentAnswer struct {
	ID              uuid.UUID  `json:"id"`
	TaskID          uuid.UUID  `json:"task_id"`
	Access          string     `json:"access"`
	Subject         *string    `json:"subject"`
	PlainText       *string    `json:"plain_text"`
	HTML            *string    `json:"html"`
	ExpiresAt       *time.Time `json:"expires_at"`
	MaskedVariables []string   `json:"masked_variables"`
	MaskedBy        *string    `json:"masked_by"`
}

func getAs(t *testing.T, app *fiber.App, path, sub string, roles ...string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, path, nil)
	request.Header.Set("X-Test-Sub", sub)
	request.Header.Set("X-Test-Roles", strings.Join(roles, ","))
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func contentAs(t *testing.T, app *fiber.App, task, row uuid.UUID, sub string, roles ...string) contentAnswer {
	t.Helper()
	status, body := getAs(t, app, "/mail_tasks/"+task.String()+"/queue/"+row.String()+"/content", sub, roles...)
	if status != fiber.StatusOK {
		t.Fatalf("content status %d: %s", status, body)
	}
	var answer contentAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

// Who sees what (Yusuf, 2026-10-12): a list send's content its sender, anyone
// who may send to lists, and whoever reads system mail; a single mail's
// content only its sender and whoever reads system mail. A sensitive mail's
// content is shown to the same people masked (2026-10-13); this one holds no
// code or link, so masked it reads as sent.
func TestSentMailContentFollowsWhoMayReadIt(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	yes, no := true, false
	recent := now.Add(-time.Hour)

	listTask, listRow := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: recent})
	singleTask, singleRow := seedContentSend(t, db, contentSend{sentBy: contentSender, createdAt: recent})
	shownTask, shownRow := seedContentSend(t, db, contentSend{sensitive: &no, sentBy: contentSender, createdAt: recent})
	markedTask, markedRow := seedContentSend(t, db, contentSend{sensitive: &yes, list: true, sentBy: contentSender, createdAt: recent})

	reader := []string{"skymail:access", "skymail:mails:read"}
	writer := []string{"skymail:access", "skymail:mails:read", "skymail:mails:write"}
	system := []string{"skymail:access", "skymail:mails:read", SystemMailsReadRole}

	cases := []struct {
		name      string
		task, row uuid.UUID
		sub       string
		roles     []string
		want      string
	}{
		{"list send, reader", listTask, listRow, contentOther, reader, "restricted"},
		{"list send, its sender", listTask, listRow, contentSender, reader, "available"},
		{"list send, list sender", listTask, listRow, contentOther, writer, "available"},
		{"list send, system reader", listTask, listRow, contentOther, system, "available"},
		{"single, sensitive by default, system reader", singleTask, singleRow, contentOther, system, "masked"},
		{"single, sensitive by default, its sender", singleTask, singleRow, contentSender, writer, "masked"},
		{"single, sensitive by default, list sender", singleTask, singleRow, contentOther, writer, "restricted"},
		{"single, sensitive by default, reader", singleTask, singleRow, contentOther, reader, "restricted"},
		{"single shown, list sender", shownTask, shownRow, contentOther, writer, "restricted"},
		{"single shown, its sender", shownTask, shownRow, contentSender, reader, "available"},
		{"single shown, system reader", shownTask, shownRow, contentOther, system, "available"},
		{"list send marked sensitive, system reader", markedTask, markedRow, contentOther, append(system, "skymail:mails:write"), "masked"},
		{"list send marked sensitive, list sender", markedTask, markedRow, contentOther, writer, "masked"},
		{"list send marked sensitive, reader", markedTask, markedRow, contentOther, reader, "restricted"},
	}
	for _, tc := range cases {
		answer := contentAs(t, app, tc.task, tc.row, tc.sub, tc.roles...)
		if answer.Access != tc.want {
			t.Errorf("%s: access %q, want %q", tc.name, answer.Access, tc.want)
			continue
		}
		if answer.ID != tc.row || answer.TaskID != tc.task {
			t.Errorf("%s: answered for %s/%s", tc.name, answer.TaskID, answer.ID)
		}
		shown := answer.Subject != nil || answer.PlainText != nil || answer.HTML != nil
		if tc.want == "available" || tc.want == "masked" {
			if !shown || *answer.Subject != "Duyuru" || *answer.PlainText != "Merhaba Ayşe" || *answer.HTML != "<p>Merhaba <b>Ayşe</b></p>" {
				t.Errorf("%s: content %+v, want the mail as sent", tc.name, answer)
			}
			if answer.ExpiresAt == nil || !answer.ExpiresAt.Equal(recent.Add(90*24*time.Hour)) {
				t.Errorf("%s: expires_at %v, want 90 days after the send", tc.name, answer.ExpiresAt)
			}
		} else if shown {
			t.Errorf("%s: %s content carried subject, text or html: %+v", tc.name, tc.want, answer)
		}
	}
}

func TestSentMailContentPastRetentionOrEmptiedIsExpired(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	writer := []string{"skymail:access", "skymail:mails:read", "skymail:mails:write"}

	// Past 90 days but not swept yet: expired all the same.
	oldTask, oldRow := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: now.Add(-91 * 24 * time.Hour)})
	// Swept.
	sweptTask, sweptRow := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: now.Add(-24 * time.Hour), cleared: true})
	for name, ids := range map[string][2]uuid.UUID{"past retention": {oldTask, oldRow}, "swept": {sweptTask, sweptRow}} {
		if answer := contentAs(t, app, ids[0], ids[1], contentOther, writer...); answer.Access != "expired" || answer.Subject != nil || answer.HTML != nil {
			t.Errorf("%s: %+v, want expired with no content", name, answer)
		}
	}

	// Emptied by Account erasure within the period: nothing to show, said so.
	task, row := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: now.Add(-time.Hour)})
	if _, err := db.Conn.Exec(context.Background(),
		`UPDATE mail_queue SET subject = '', body = '', body_html = NULL WHERE id = $1`, row); err != nil {
		t.Fatal(err)
	}
	if answer := contentAs(t, app, task, row, contentOther, writer...); answer.Access != "unavailable" || answer.Subject != nil {
		t.Errorf("an erased row: %+v, want unavailable", answer)
	}

	// A row of another send, or none at all: 404.
	_, otherRow := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: now.Add(-time.Hour)})
	for _, path := range []string{
		"/mail_tasks/" + task.String() + "/queue/" + otherRow.String() + "/content",
		"/mail_tasks/" + task.String() + "/queue/" + uuid.NewString() + "/content",
	} {
		if status, body := getAs(t, app, path, contentOther, writer...); status != fiber.StatusNotFound {
			t.Errorf("GET %s = %d %s, want 404", path, status, body)
		}
	}
	if status, _ := getAs(t, app, "/mail_tasks/"+task.String()+"/queue/not-a-uuid/content", contentOther, writer...); status == fiber.StatusOK {
		t.Error("a malformed row id answered 200")
	}
}

// A send says whether its content can be opened, and its values — a code or a
// link for Keycloak's mails — travel only with content the caller may read.
func TestSendCarriesContentAccessAndHidesValuesItCannotShow(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	recent := now.Add(-time.Hour)
	listTask, _ := seedContentSend(t, db, contentSend{list: true, sentBy: contentSender, createdAt: recent})
	singleTask, _ := seedContentSend(t, db, contentSend{sentBy: contentSender, createdAt: recent})
	writer := []string{"skymail:access", "skymail:mails:read", "skymail:mails:write"}
	reader := []string{"skymail:access", "skymail:mails:read"}

	type send struct {
		ID               uuid.UUID       `json:"id"`
		ContentAccess    string          `json:"content_access"`
		ContentExpiresAt *time.Time      `json:"content_expires_at"`
		BodyVariables    json.RawMessage `json:"body_variables"`
	}
	read := func(path string, roles []string) map[uuid.UUID]send {
		status, body := getAs(t, app, path, contentOther, roles...)
		if status != fiber.StatusOK {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
		var sends []send
		if strings.HasPrefix(string(body), "{") {
			var one send
			if err := json.Unmarshal(body, &one); err != nil {
				t.Fatal(err)
			}
			sends = []send{one}
		} else if err := json.Unmarshal(body, &sends); err != nil {
			t.Fatal(err)
		}
		byID := map[uuid.UUID]send{}
		for _, s := range sends {
			byID[s.ID] = s
		}
		return byID
	}

	list := read("/mail_tasks", writer)
	if got := list[listTask]; got.ContentAccess != "available" || string(got.BodyVariables) == "null" ||
		got.ContentExpiresAt == nil || !got.ContentExpiresAt.Equal(recent.Add(90*24*time.Hour)) {
		t.Errorf("list send to a list sender: %+v, want available with its values and when it expires", got)
	}
	if got := list[singleTask]; got.ContentAccess != "restricted" || string(got.BodyVariables) != "null" || got.ContentExpiresAt != nil {
		t.Errorf("single mail to a list sender: %+v, want restricted, no values, no expiry", got)
	}
	// Its sender opens it masked: its values, which may hold its code or its
	// link, are not listed.
	status, body := getAs(t, app, "/mail_tasks/"+singleTask.String(), contentSender, reader...)
	var own send
	if err := json.Unmarshal(body, &own); status != fiber.StatusOK || err != nil {
		t.Fatalf("GET own single mail = %d %s", status, body)
	}
	if own.ContentAccess != "masked" || string(own.BodyVariables) != "null" || own.ContentExpiresAt == nil {
		t.Errorf("single mail to its sender: %+v, want masked, no values, an expiry", own)
	}
	if got := read("/mail_tasks/"+listTask.String(), reader)[listTask]; got.ContentAccess != "restricted" || string(got.BodyVariables) != "null" {
		t.Errorf("list send to a reader: %+v, want restricted with no values", got)
	}
}
