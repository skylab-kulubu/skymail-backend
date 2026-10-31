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

// A send's controls (duyuru-haber 05 G4): POST /v1/mail_tasks/{id}/pause,
// /resume and /cancel, as production registers them.

const (
	controlSender = "31ef736f-72da-4a40-8791-d523199cf9f0"
	controlOther  = "8a3b2c1d-0000-4000-8000-000000000099"
	controlList   = "40000000-0000-4000-8000-00000000000a"
	controlSingle = "40000000-0000-4000-8000-00000000000b"
	controlDone   = "40000000-0000-4000-8000-00000000000c"
)

// controlApp serves the routes for a caller sub with roles, on a database
// holding a list send by controlSender (two pending rows, one processing, one
// sent), a single mail and a list send whose rows are all sent.
func controlApp(t *testing.T, sub string, roles ...string) (*fiber.App, *database.Store) {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('10000000-0000-4000-8000-000000000001', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', '');
		INSERT INTO mailing_lists (id, name) VALUES ('20000000-0000-4000-8000-000000000001', 'Tüm üyeler');
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables) VALUES
		    ('`+controlList+`', '`+controlSender+`', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}'),
		    ('`+controlDone+`', '`+controlSender+`', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES
		    ('`+controlSingle+`', '`+controlSender+`', '10000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority, claimed_at, claimed_by) VALUES
		    ('`+controlList+`', 'A', 'a@example.com', 'Duyuru', 'x', 'pending', 0, NULL, NULL),
		    ('`+controlList+`', 'B', 'b@example.com', 'Duyuru', 'x', 'pending', 0, NULL, NULL),
		    ('`+controlList+`', 'C', 'c@example.com', 'Duyuru', 'x', 'processing', 0, now(), 'replica'),
		    ('`+controlList+`', 'D', 'd@example.com', 'Duyuru', 'x', 'sent', 0, NULL, NULL),
		    ('`+controlDone+`', 'E', 'e@example.com', 'Duyuru', 'x', 'sent', 0, NULL, NULL),
		    ('`+controlSingle+`', 'F', 'f@example.com', 'Kod', 'x', 'pending', 1, NULL, NULL);`); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", sub)
		c.Locals("roles", roles)
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	return app, store
}

type controlAnswer struct {
	Status          int
	Code            string
	PausedAt        *string `json:"paused_at"`
	PausedReason    *string `json:"paused_reason"`
	CancelledAt     *string `json:"cancelled_at"`
	SendStatus      string  `json:"status"`
	RecipientCounts struct {
		Pending   int `json:"pending"`
		Failed    int `json:"failed"`
		Cancelled int `json:"cancelled"`
	} `json:"recipient_counts"`
}

func control(t *testing.T, app *fiber.App, task, action string) controlAnswer {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/v1/mail_tasks/"+task+"/"+action, nil))
	if err != nil {
		t.Fatal(err)
	}
	var answer controlAnswer
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	answer.Status = response.StatusCode
	return answer
}

func TestASendersOwnSendCanBePausedResumedAndCancelled(t *testing.T) {
	app, store := controlApp(t, controlSender, "skymail:access", "skymail:mails:write", "skymail:mails:read")

	paused := control(t, app, controlList, "pause")
	if paused.Status != fiber.StatusOK || paused.PausedAt == nil || paused.PausedReason == nil || *paused.PausedReason != "operator" {
		t.Fatalf("pause answered %+v, want 200 with paused_at and paused_reason operator", paused)
	}
	if again := control(t, app, controlList, "pause"); again.Status != fiber.StatusOK || *again.PausedAt != *paused.PausedAt {
		t.Fatalf("pausing a paused send answered %+v, want 200 with the same paused_at", again)
	}
	parked := func() int {
		var n int
		if err := store.Conn.QueryRow(context.Background(),
			`SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status = 'pending' AND next_attempt_at >= '2999-01-01'`, controlList).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := parked(); n != 2 {
		t.Fatalf("the pause parked %d of the 2 pending rows", n)
	}
	if resumed := control(t, app, controlList, "resume"); resumed.Status != fiber.StatusOK || resumed.PausedAt != nil || resumed.PausedReason != nil {
		t.Fatalf("resume answered %+v, want 200 with no pause", resumed)
	}
	if n := parked(); n != 0 {
		t.Fatalf("after the resume %d rows are still parked", n)
	}

	cancelled := control(t, app, controlList, "cancel")
	if cancelled.Status != fiber.StatusOK || cancelled.CancelledAt == nil {
		t.Fatalf("cancel answered %+v, want 200 with cancelled_at", cancelled)
	}
	if c := cancelled.RecipientCounts; c.Pending != 0 || c.Cancelled != 2 || c.Failed != 2 {
		t.Fatalf("after the cancel the counts are %+v, want the 2 pending rows cancelled (in failed too)", c)
	}
	if cancelled.SendStatus == "failed" {
		t.Fatal("a cancelled send with one recipient sent reads failed")
	}
	var processing int
	if err := store.Conn.QueryRow(context.Background(),
		`SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status = 'processing'`, controlList).Scan(&processing); err != nil || processing != 1 {
		t.Fatalf("the row being sent at the cancel: %d processing, %v; want it left to finish", processing, err)
	}
	for _, action := range []string{"pause", "resume"} {
		if got := control(t, app, controlList, action); got.Status != fiber.StatusConflict || got.Code != "mail_task.cancelled" {
			t.Fatalf("%s of a cancelled send answered %+v, want 409 mail_task.cancelled", action, got)
		}
	}
	if again := control(t, app, controlList, "cancel"); again.Status != fiber.StatusOK {
		t.Fatalf("cancelling a cancelled send answered %+v, want 200", again)
	}
}

func TestOnlyABulkUnfinishedSendHasControls(t *testing.T) {
	app, _ := controlApp(t, controlSender, "skymail:access", "skymail:mails:write")
	for _, action := range []string{"pause", "resume", "cancel"} {
		if got := control(t, app, controlSingle, action); got.Status != fiber.StatusConflict || got.Code != "mail_task.not_bulk" {
			t.Fatalf("%s of a single mail answered %+v, want 409 mail_task.not_bulk", action, got)
		}
	}
	for _, action := range []string{"pause", "cancel"} {
		if got := control(t, app, controlDone, action); got.Status != fiber.StatusConflict || got.Code != "mail_task.finished" {
			t.Fatalf("%s of a finished send answered %+v, want 409 mail_task.finished", action, got)
		}
	}
	if got := control(t, app, "40000000-0000-4000-8000-0000000000ff", "pause"); got.Status != fiber.StatusNotFound {
		t.Fatalf("pause of an unknown send answered %+v, want 404", got)
	}
}

func TestWhoMayControlASend(t *testing.T) {
	// Another list sender may not touch the send.
	app, _ := controlApp(t, controlOther, "skymail:access", "skymail:mails:write")
	if got := control(t, app, controlList, "cancel"); got.Status != fiber.StatusForbidden {
		t.Fatalf("another list sender's cancel answered %+v, want 403", got)
	}
	// Reading sends is not enough.
	app, _ = controlApp(t, controlSender, "skymail:access", "skymail:mails:read")
	if got := control(t, app, controlList, "pause"); got.Status != fiber.StatusForbidden {
		t.Fatalf("a reader's pause answered %+v, want 403", got)
	}
	// Who may send to everyone controls any send.
	app, _ = controlApp(t, controlOther, "skymail:access", handlers.AnnouncementsSendRole)
	if got := control(t, app, controlList, "pause"); got.Status != fiber.StatusOK || got.PausedAt == nil {
		t.Fatalf("an announcements sender's pause answered %+v, want 200", got)
	}
}
