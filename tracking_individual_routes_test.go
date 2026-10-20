package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// Tracking in a mail sent to people one by one (Yusuf, 2026-10-20): the
// compose form's people (POST /mail_tasks/single) and a request for approval
// to people may ask for it, as a list send does. Each recipient is decided by
// the same rule: SkyMail knows these people only by their address, so each
// row says no_account and nobody is tracked; an address is never looked up to
// find an account. A service's single mail — account and transaction mail —
// is never tracked, nor a System or sensitive template.

// setFreeBasic sets free.basic's System flag and content_sensitive (nil: the
// template says nothing).
func (w *approvalWorld) setFreeBasic(system bool, sensitive *bool) {
	w.t.Helper()
	if _, err := w.store.Conn.Exec(context.Background(), `UPDATE templates SET system = $2, content_sensitive = $3 WHERE id = $1`,
		w.freeBasic.ID, system, sensitive); err != nil {
		w.t.Fatal(err)
	}
}

// singleSend is free.basic to Ayşe alone, as the compose form sends one of
// its people.
func (w *approvalWorld) singleSend(tracked bool) map[string]any {
	send := w.listSend()
	delete(send, "mail_list_id")
	send["recipient_email"] = ayseKaya.Email
	send["recipient_full_name"] = ayseKaya.FullName
	if tracked {
		send["tracking"] = true
	}
	return send
}

// laneOf is the lane every queue row of task is in, and how many rows it has.
func (w *approvalWorld) laneOf(task uuid.UUID) (mailer.Priority, int) {
	w.t.Helper()
	var lane, rows int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT coalesce(max(priority), -1), count(*) FROM mail_queue WHERE task_id = $1`, task).Scan(&lane, &rows); err != nil {
		w.t.Fatal(err)
	}
	return mailer.Priority(lane), rows
}

type createdSend struct {
	ID       uuid.UUID `json:"id"`
	Tracking *bool     `json:"tracking"`
}

// A person's single send takes tracking. On, it says tracking true, goes in
// the bulk lane, and its one row says no_account: Ayşe is known by address
// only. Off, the field is ignored and the mail goes as it always has. Without
// the field it goes in the system lane, untracked. Core is never asked.
func TestAPersonsSingleSendTakesTracking(t *testing.T) {
	notSensitive := false
	for _, on := range []bool{true, false} {
		w, prefs := trackedWorld(t, on)
		w.setFreeBasic(false, &notSensitive)

		var created createdSend
		status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks/single", w.singleSend(true), &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking != on {
			t.Fatalf("on=%v: POST /v1/mail_tasks/single = %d %s, want 201 with tracking %v", on, status, raw, on)
		}
		requested, rows := w.trackingOf(created.ID)
		if requested != on {
			t.Errorf("on=%v: tracking_requested %v", on, requested)
		}
		row, ok := rows[ayseKaya.Email]
		if !ok || len(rows) != 1 {
			t.Fatalf("on=%v: rows %+v, want Ayşe's alone", on, rows)
		}
		if row.sub != "" || row.token != nil {
			t.Errorf("on=%v: Ayşe's row %+v: an address must never be given an account or a token", on, row)
		}
		wantReason, wantLane := "", mailer.PrioritySystem
		if on {
			wantReason, wantLane = tracking.SkipNoAccount, mailer.PriorityBulk
		}
		if row.reason != wantReason {
			t.Errorf("on=%v: Ayşe's row says %q, want %q", on, row.reason, wantReason)
		}
		if lane, _ := w.laneOf(created.ID); lane != wantLane {
			t.Errorf("on=%v: the send is in lane %d, want %d", on, lane, wantLane)
		}

		// Without the field: the system lane, untracked, as before.
		status, raw = w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks/single", w.singleSend(false), &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking {
			t.Fatalf("on=%v: a single send without tracking = %d %s, want tracking false", on, status, raw)
		}
		if requested, rows := w.trackingOf(created.ID); requested || rows[ayseKaya.Email].reason != "" {
			t.Errorf("on=%v: a single send without tracking asked for it: %v %+v", on, requested, rows)
		}
		if lane, _ := w.laneOf(created.ID); lane != mailer.PrioritySystem {
			t.Errorf("on=%v: a single send without tracking is in lane %d, want the system lane", on, lane)
		}
		if prefs.asked != 0 {
			t.Errorf("on=%v: core asked %d times about people known only by address", on, prefs.asked)
		}
	}
}

// A single send of a template that may never be tracked says tracking false
// and goes in the system lane: a System template, a sensitive one, and — to
// one person — one that says nothing of its content, which single mail reads
// as sensitive.
func TestASingleSendThatCannotBeTrackedSaysSo(t *testing.T) {
	w, prefs := trackedWorld(t, true)
	sensitive, notSensitive := true, false
	for _, c := range []struct {
		name      string
		system    bool
		sensitive *bool
	}{
		{"System", true, &notSensitive},
		{"sensitive", false, &sensitive},
		{"says nothing", false, nil},
	} {
		w.setFreeBasic(c.system, c.sensitive)
		var created createdSend
		status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks/single", w.singleSend(true), &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking {
			t.Fatalf("%s: POST /v1/mail_tasks/single = %d %s, want tracking false", c.name, status, raw)
		}
		if requested, rows := w.trackingOf(created.ID); requested || rows[ayseKaya.Email].reason != "" {
			t.Errorf("%s: the send says tracking %v, rows %+v", c.name, requested, rows)
		}
		if lane, _ := w.laneOf(created.ID); lane != mailer.PrioritySystem {
			t.Errorf("%s: the send is in lane %d, want the system lane", c.name, lane)
		}
	}
	if prefs.asked != 0 {
		t.Errorf("core asked %d times", prefs.asked)
	}
}

// A request for approval to people takes tracking as one to a list does; its
// approved sends, one a person, ask for it, and each row says no_account.
// A template that says nothing of its content is sensitive for people: the
// request says tracking false.
func TestARequestToPeopleTakesTracking(t *testing.T) {
	notSensitive := false
	for _, on := range []bool{true, false} {
		w, prefs := trackedWorld(t, on)
		w.setFreeBasic(false, &notSensitive)
		send := w.peopleSend(ayseKaya, mehmetDemir)
		send["tracking"] = true
		var submitted map[string]any
		status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &submitted)
		if status != fiber.StatusCreated || submitted["tracking"] != on {
			t.Fatalf("on=%v: submit = %d %s, want tracking %v", on, status, raw, on)
		}
		id := uuid.MustParse(submitted["id"].(string))
		status, answer, failure := w.act("fatih", id, "approve", nil)
		if status != fiber.StatusOK || len(answer.TaskIDs) != 2 {
			t.Fatalf("on=%v: approve = %d %+v %+v", on, status, answer.TaskIDs, failure)
		}
		for _, task := range answer.TaskIDs {
			requested, rows := w.trackingOf(task)
			if requested != on || len(rows) != 1 {
				t.Errorf("on=%v: send %s tracking_requested %v, rows %+v", on, task, requested, rows)
			}
			for email, row := range rows {
				want := ""
				if on {
					want = tracking.SkipNoAccount
				}
				if row.sub != "" || row.token != nil || row.reason != want {
					t.Errorf("on=%v: %s's row %+v, want no account, no token, reason %q", on, email, row, want)
				}
			}
			if lane, _ := w.laneOf(task); lane != mailer.PriorityBulk {
				t.Errorf("on=%v: an approved send is in lane %d, want the bulk lane", on, lane)
			}
		}
		if prefs.asked != 0 {
			t.Errorf("on=%v: core asked %d times about people known only by address", on, prefs.asked)
		}
	}

	w, _ := trackedWorld(t, true)
	send := w.peopleSend(ayseKaya)
	send["tracking"] = true
	var submitted map[string]any
	if status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &submitted); status != fiber.StatusCreated || submitted["tracking"] != false {
		t.Fatalf("a request to people of a template that says nothing = %d %s, want tracking false", status, raw)
	}
}

// singleSendWorld is POST /v1/mail_tasks/single through the chain production
// registers — userinfo, skymail:access, the role, the handler — with tracking
// on, so a service's token and a person's are told apart as Authenticate
// accepted them.
func singleSendWorld(t *testing.T) (*fiber.App, *database.Store, uuid.UUID) {
	t.Helper()
	w, _ := trackedWorld(t, true)
	notSensitive := false
	w.setFreeBasic(false, &notSensitive)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	return app, w.store, w.freeBasic.ID
}

// A service's single mail is account or transaction mail: it is never
// tracked, though it asks, and stays in the system lane. A person's token
// through the same chain gets it.
func TestAServicesSingleSendIsNeverTracked(t *testing.T) {
	app, store, template := singleSendWorld(t)
	for _, c := range []struct {
		name    string
		service bool
	}{{"service", true}, {"person", false}} {
		body, _ := json.Marshal(map[string]any{
			"template_id": template, "recipient_email": ayseKaya.Email, "recipient_full_name": ayseKaya.FullName,
			"body_variables": map[string]any{"Subject": "Konu", "Heading": "Başlık", "BodyHtml": "<p>Metin</p>"},
			"tracking":       true,
		})
		request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer(t, uuid.NewString(), c.service, "skymail:access", "skymail:mails:send"))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		var created createdSend
		if response.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Tracking == nil || *created.Tracking == c.service {
			t.Fatalf("%s: POST /v1/mail_tasks/single = %d %s, want tracking %v", c.name, response.StatusCode, raw, !c.service)
		}
		var requested bool
		var lane int
		if err := store.Conn.QueryRow(context.Background(), `
			SELECT mt.tracking_requested, mq.priority FROM mail_tasks mt JOIN mail_queue mq ON mq.task_id = mt.id WHERE mt.id = $1`,
			created.ID).Scan(&requested, &lane); err != nil {
			t.Fatal(err)
		}
		wantLane := mailer.PriorityBulk
		if c.service {
			wantLane = mailer.PrioritySystem
		}
		if requested == c.service || mailer.Priority(lane) != wantLane {
			t.Errorf("%s: tracking_requested %v in lane %d, want %v in lane %d", c.name, requested, lane, !c.service, wantLane)
		}
	}
}
