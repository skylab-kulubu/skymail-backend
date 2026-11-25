package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// A send's events (skymail-yeni-tasarim B3): GET /v1/mail_tasks/{id}/events,
// as production registers it.

const (
	eventsSender = "31ef736f-72da-4a40-8791-d523199cf9f0"
	eventsGone   = "8a3b2c1d-0000-4000-8000-0000000000ee"
	eventsSend   = "40000000-0000-4000-8000-0000000000a1"
	eventsByGone = "40000000-0000-4000-8000-0000000000a2"
	eventsBySelf = "40000000-0000-4000-8000-0000000000a3"
)

// namedPeople is Keycloak naming the send's sender; anyone else is not there.
type namedPeople struct{ noGroups }

func (namedPeople) GetUser(_ context.Context, id string) (*gocloak.User, error) {
	if id != eventsSender {
		return nil, nil
	}
	first, last := "Ece", "Demir"
	return &gocloak.User{ID: &id, FirstName: &first, LastName: &last}, nil
}

type sendEvent struct {
	Kind  string     `json:"kind"`
	At    time.Time  `json:"at"`
	Until *time.Time `json:"until"`
	Actor *struct {
		Kind    string  `json:"kind"`
		Sub     *string `json:"sub"`
		Name    *string `json:"name"`
		Deleted bool    `json:"deleted"`
	} `json:"actor"`
	Reason     *string          `json:"reason"`
	Note       *string          `json:"note"`
	ApprovalID *uuid.UUID       `json:"approval_id"`
	Counts     map[string]int64 `json:"counts"`
}

func eventKinds(events []sendEvent) string {
	kinds := make([]string, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	return strings.Join(kinds, ",")
}

func eventOf(events []sendEvent, kind string) *sendEvent {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

// eventsApp serves the send routes to the caller each request names
// (X-Test-Sub, X-Test-Name, X-Test-Roles), on a database holding a list send
// by eventsSender — two pending rows, one sent after a retry, one skipped
// for a suppressed address — a send by an account Keycloak no longer has and
// a notice SkyMail sent.
func eventsApp(t *testing.T) (*fiber.App, *database.Store) {
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
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, created_at) VALUES
		    ('`+eventsSend+`', '`+eventsSender+`', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}', now() - interval '1 hour'),
		    ('`+eventsByGone+`', '`+eventsGone+`', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}', now()),
		    ('`+eventsBySelf+`', 'skymail', '10000000-0000-4000-8000-000000000001', NULL, '{}', now());
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, priority, error, attempts, sent_at, created_at) VALUES
		    ('50000000-0000-4000-8000-0000000000a1', '`+eventsSend+`', 'A', 'a@example.com', 'Duyuru', 'x', 'pending', 0, NULL, 0, NULL, now() - interval '59 minutes'),
		    ('50000000-0000-4000-8000-0000000000a2', '`+eventsSend+`', 'B', 'b@example.com', 'Duyuru', 'x', 'pending', 0, NULL, 0, NULL, now() - interval '59 minutes'),
		    ('50000000-0000-4000-8000-0000000000a3', '`+eventsSend+`', 'C', 'c@example.com', 'Duyuru', 'x', 'sent', 0, NULL, 1, now() - interval '30 minutes', now() - interval '59 minutes'),
		    ('50000000-0000-4000-8000-0000000000a4', '`+eventsSend+`', 'D', 'd@example.com', 'Duyuru', 'x', 'failed', 0, 'suppressed: hard_bounce', 0, NULL, now() - interval '59 minutes');`); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", c.Get("X-Test-Sub"))
		c.Locals("user_name", c.Get("X-Test-Name"))
		c.Locals("roles", strings.Split(c.Get("X-Test-Roles"), ","))
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, namedPeople{}))
	return app, store
}

func eventsCall(t *testing.T, app *fiber.App, method, path, roles string, out any) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("X-Test-Sub", eventsSender)
	request.Header.Set("X-Test-Name", "Ece Demir")
	request.Header.Set("X-Test-Roles", roles)
	response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if out != nil && response.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	return response.StatusCode, string(raw)
}

const eventsReader = "skymail:access,skymail:mails:read"

// The send's life on one line: created by its sender as Keycloak names
// them, queued to four; then paused, resumed and cancelled by hand, each
// with who did it and the name their token carried; once no recipient is
// left, finished with its counts; a hard bounce reported after sending.
func TestASendsEventsTellItsLife(t *testing.T) {
	app, store := eventsApp(t)
	path := "/v1/mail_tasks/" + eventsSend + "/events"

	var events []sendEvent
	if status, raw := eventsCall(t, app, fiber.MethodGet, path, eventsReader, &events); status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	if got := eventKinds(events); got != "created,queued" {
		t.Fatalf("a send going out tells %s, want created,queued", got)
	}
	created := events[0]
	if created.Actor == nil || created.Actor.Kind != "person" || created.Actor.Sub == nil || *created.Actor.Sub != eventsSender ||
		created.Actor.Name == nil || *created.Actor.Name != "Ece Demir" || created.Actor.Deleted {
		t.Errorf("created actor = %+v", created.Actor)
	}
	if queued := events[1]; queued.Counts["recipients"] != 4 || queued.Actor != nil || !queued.At.After(created.At) {
		t.Errorf("queued = %+v", queued)
	}

	controller := "skymail:access,skymail:mails:write"
	for _, action := range []string{"pause", "pause", "resume", "cancel", "cancel"} {
		if status, raw := eventsCall(t, app, fiber.MethodPost, "/v1/mail_tasks/"+eventsSend+"/"+action, controller, nil); status != 200 {
			t.Fatalf("%s: %d %s", action, status, raw)
		}
	}
	// A bounce report for the sent row, after it went out.
	if _, err := store.Conn.Exec(context.Background(), `
		INSERT INTO mail_bounces (queue_id, source, kind, action, recipient_matched, ndr_id_sha256, received_at)
		VALUES ('50000000-0000-4000-8000-0000000000a3', 'dsn', 'hard', 'failed', true, repeat('a', 64), now() + interval '1 second')`); err != nil {
		t.Fatal(err)
	}

	events = nil
	if status, raw := eventsCall(t, app, fiber.MethodGet, path, eventsReader, &events); status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	// A second pause and a second cancel changed nothing: no event.
	if got := eventKinds(events); got != "created,queued,paused,resumed,cancelled,finished,bounced" {
		t.Fatalf("events = %s", got)
	}
	for _, kind := range []string{"paused", "resumed", "cancelled"} {
		e := eventOf(events, kind)
		if e.Actor == nil || e.Actor.Sub == nil || *e.Actor.Sub != eventsSender || e.Actor.Name == nil || *e.Actor.Name != "Ece Demir" {
			t.Errorf("%s actor = %+v", kind, e.Actor)
		}
	}
	if paused := eventOf(events, "paused"); paused.Reason == nil || *paused.Reason != "operator" {
		t.Errorf("paused reason = %v", paused.Reason)
	}
	if cancelled := eventOf(events, "cancelled"); cancelled.Counts["cancelled"] != 2 {
		t.Errorf("cancelled counts = %v, want the two pending recipients", cancelled.Counts)
	}
	finished := eventOf(events, "finished")
	want := map[string]int64{"sent": 1, "failed": 0, "skipped": 1, "cancelled": 2, "retries": 1}
	for key, n := range want {
		if finished.Counts[key] != n {
			t.Errorf("finished counts = %v, want %v", finished.Counts, want)
			break
		}
	}
	if finished.Actor != nil {
		t.Errorf("finished has an actor: %+v", finished.Actor)
	}
	if bounced := eventOf(events, "bounced"); bounced.Counts["bounced"] != 1 {
		t.Errorf("bounced counts = %v", bounced.Counts)
	}
}

// Who queued a send: an account Keycloak no longer has, or one erased, reads
// as Silinmiş kullanıcı; SkyMail's own notice names SkyMail; a pause written
// before the events were (the send's own columns) still tells who.
func TestASendsEventsNameEveryActor(t *testing.T) {
	app, store := eventsApp(t)
	ctx := context.Background()

	var events []sendEvent
	if status, raw := eventsCall(t, app, fiber.MethodGet, "/v1/mail_tasks/"+eventsByGone+"/events", eventsReader, &events); status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	if a := events[0].Actor; a == nil || !a.Deleted || a.Name == nil || *a.Name != database.DeletedUserName {
		t.Errorf("a sender Keycloak no longer has = %+v, want Silinmiş kullanıcı", a)
	}
	// A send with no recipient queued yet tells only that it was created.
	if got := eventKinds(events); got != "created" {
		t.Errorf("events = %s", got)
	}

	events = nil
	if status, raw := eventsCall(t, app, fiber.MethodGet, "/v1/mail_tasks/"+eventsBySelf+"/events", eventsReader, &events); status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	if a := events[0].Actor; a == nil || a.Kind != "system" || a.Sub != nil || a.Name == nil || *a.Name != "SkyMail" {
		t.Errorf("SkyMail's own send = %+v", a)
	}

	if _, err := store.Conn.Exec(ctx, `
		UPDATE mail_tasks SET sent_by = $2, paused_at = now(), paused_reason = 'operator', paused_by = $3 WHERE id = $1`,
		eventsSend, database.DeletedUserSubject, eventsSender); err != nil {
		t.Fatal(err)
	}
	events = nil
	if status, raw := eventsCall(t, app, fiber.MethodGet, "/v1/mail_tasks/"+eventsSend+"/events", eventsReader, &events); status != 200 {
		t.Fatalf("events: %d %s", status, raw)
	}
	if a := events[0].Actor; a == nil || !a.Deleted || a.Sub == nil || *a.Sub != database.DeletedUserSubject {
		t.Errorf("an erased sender = %+v", a)
	}
	paused := eventOf(events, "paused")
	if paused == nil || paused.Actor == nil || paused.Actor.Name == nil || *paused.Actor.Name != "Ece Demir" {
		t.Fatalf("a pause from before the events: %+v", paused)
	}
}

// skymail:mails:read reads any send's events; nothing less does. An unknown
// send is 404 and a malformed id 400, both problems whose status is the
// number.
func TestASendsEventsTakeMailsRead(t *testing.T) {
	app, _ := eventsApp(t)
	if status, raw := eventsCall(t, app, fiber.MethodGet, "/v1/mail_tasks/"+eventsSend+"/events", "skymail:access,skymail:mails:write", nil); status != 403 {
		t.Errorf("without mails:read: %d %s", status, raw)
	}
	for path, want := range map[string]int{
		"/v1/mail_tasks/" + uuid.NewString() + "/events": 404,
		"/v1/mail_tasks/bozuk/events":                    400,
	} {
		status, raw := eventsCall(t, app, fiber.MethodGet, path, eventsReader, nil)
		var problem struct {
			Status int `json:"status"`
		}
		if err := json.Unmarshal([]byte(raw), &problem); err != nil {
			t.Fatal(err)
		}
		if status != want || problem.Status != want {
			t.Errorf("%s: %d %s, want %d with status %d", path, status, raw, want, want)
		}
	}
}
