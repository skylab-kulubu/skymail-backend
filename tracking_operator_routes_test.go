package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// Operator tracking through the routes (SKYMAIL_TRACKING_OPERATOR, Yusuf
// 2026-10-21): with it on, every send a person makes in the panel — to one
// person, a group, a list, through Mail onayı — is tracked whether it asked
// or not, whatever its template; the summary says so, and the send form
// shows no box. A service's mail is not a person's send: SKYMAIL_TRACKING_SYSTEM
// alone decides it.

// operatorTrackedWorld is accountsWorld with SKYMAIL_TRACKING_OPERATOR on and
// system tracking off.
func operatorTrackedWorld(t *testing.T) (*approvalWorld, *routePreferences) {
	t.Helper()
	prefs := &routePreferences{}
	accounts := &laterAccounts{}
	w := newApprovalWorldWithPool(t, 0, mailer.WithTracking(mailer.Tracking{
		Config: tracking.Config{Mode: tracking.ModeOn, Operator: tracking.ModeOn,
			BaseURL: "https://api.example.test/api/skymail", CoreURL: "http://core.invalid"},
		Preferences: prefs,
		Accounts:    accounts,
	}))
	prefs.store = w.store
	accounts.directory = w.directory
	notSensitive := false
	w.setFreeBasic(false, &notSensitive)
	w.account(elif, true, true)
	w.account(baska, true, true)
	w.directory.store = w.store
	return w, prefs
}

func summaryAlways(t *testing.T, w *approvalWorld) string {
	t.Helper()
	var summary map[string]json.RawMessage
	status, raw := w.call("baska", fiber.MethodGet, "/v1/mail_tasks/summary", nil, &summary)
	if status != fiber.StatusOK {
		t.Fatalf("summary = %d %s", status, raw)
	}
	return string(summary["tracking_always"])
}

// The summary says tracking_always while every person's send is tracked,
// and false otherwise (the send form then shows its box as before).
func TestTheSummarySaysWhetherEveryPersonsSendIsTracked(t *testing.T) {
	w, _ := operatorTrackedWorld(t)
	if got := summaryAlways(t, w); got != "true" {
		t.Errorf("operator tracking on: tracking_always %s", got)
	}
	for _, on := range []bool{true, false} {
		w, _ := trackedWorld(t, on)
		if got := summaryAlways(t, w); got != "false" {
			t.Errorf("tracking %v, operator off: tracking_always %s", on, got)
		}
	}
}

// A person's single send is tracked whether it asks or not — tracking
// false is ignored too — and whatever free.basic says of itself (may be
// shown, System, sensitive, nothing): in the bulk lane, Elif's account found
// by address, a token from core. The link in its body comes in a variable
// and is never registered. The answer says tracking true.
func TestEveryPersonsSingleSendIsTrackedWithOperatorTracking(t *testing.T) {
	w, prefs := operatorTrackedWorld(t)
	sensitive := true
	for _, c := range []struct {
		name      string
		system    bool
		sensitive *bool
	}{{"System", true, nil}, {"sensitive", false, &sensitive}, {"says nothing", false, nil}, {"may be shown", false, new(bool)}} {
		w.setFreeBasic(c.system, c.sensitive)
		for _, asks := range []any{nil, false, true} {
			send := w.singleSendTo(elif.email, "Elif", false)
			if asks != nil {
				send["tracking"] = asks
			}
			send["body_variables"].(map[string]any)["BodyHtml"] = `<p><a href="https://forms.example.test/r/duzenle?token=tek-kullanimlik">Düzenle</a></p>`
			created := w.sendSingle(send)
			requested, row := w.onlyRow(created.ID)
			if !*created.Tracking || !requested || w.laneOf(created.ID) != mailer.PriorityBulk {
				t.Errorf("%s, asks %v: answer %v, tracking_requested %v, lane %d; want tracked in the bulk lane",
					c.name, asks, *created.Tracking, requested, w.laneOf(created.ID))
			}
			if row.sub != elif.sub || len(row.token) != tracking.RecipientTokenBytes {
				t.Errorf("%s, asks %v: row %+v; want Elif's account and a token", c.name, asks, row)
			}
			if n := w.linkCount(created.ID); n != 0 {
				t.Errorf("%s, asks %v: %d links registered; the body's link is a variable's", c.name, asks, n)
			}
		}
	}
	if prefs.asked != 12 || w.directory.accountAsks != 12 {
		t.Errorf("core asked %d times, Keycloak %d; want 12 each", prefs.asked, w.directory.accountAsks)
	}
}

// A send to a Keycloak group and to an internal list that does not ask is
// tracked: the group's members as core says, the list's addresses
// no_account. The send list says tracking true.
func TestEveryPersonsSendToManyIsTrackedWithOperatorTracking(t *testing.T) {
	w, _ := operatorTrackedWorld(t)
	for _, audience := range []uuid.UUID{w.group, w.list.ID} {
		send := w.listSend()
		send["mail_list_id"] = audience
		var created createdSend
		status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks", send, &created)
		if status != fiber.StatusCreated || created.Tracking == nil || !*created.Tracking {
			t.Fatalf("POST /v1/mail_tasks to %s = %d %s; want tracking true", audience, status, raw)
		}
		requested, rows := w.trackingOf(created.ID)
		if !requested || w.laneOf(created.ID) != mailer.PriorityBulk {
			t.Errorf("%s: tracking_requested %v, lane %d", audience, requested, w.laneOf(created.ID))
		}
		if audience == w.group {
			if len(rows[elif.email].token) != tracking.RecipientTokenBytes || rows[baska.email].reason != tracking.SkipPreferenceOff {
				t.Errorf("group: rows %+v; want Elif tracked, Başka preference_off", rows)
			}
		} else {
			for email, row := range rows {
				if row.token != nil || row.reason != tracking.SkipNoAccount {
					t.Errorf("list, %s: %+v; want no_account", email, row)
				}
			}
		}
		var item map[string]any
		if status, raw := w.call("baska", fiber.MethodGet, "/v1/mail_tasks/"+created.ID.String(), nil, &item); status != fiber.StatusOK || item["tracking"] != true {
			t.Errorf("GET /v1/mail_tasks/%s = %d %s; want tracking true", created.ID, status, raw)
		}
	}
}

// A request for approval that does not ask is tracked: the request says
// tracking true, and the send it becomes once approved is tracked (Elif by
// her account, Ayşe no_account), in the bulk lane.
func TestARequestThatDoesNotAskIsTrackedWithOperatorTracking(t *testing.T) {
	w, _ := operatorTrackedWorld(t)
	var submitted map[string]any
	status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", w.peopleSend(ayseKaya, approvalRecipient{Email: elif.email, FullName: "Elif Yıldız"}), &submitted)
	if status != fiber.StatusCreated || submitted["tracking"] != true {
		t.Fatalf("submit = %d %s; want tracking true", status, raw)
	}
	status, answer, failure := w.act("fatih", uuid.MustParse(submitted["id"].(string)), "approve", nil)
	if status != fiber.StatusOK || len(answer.TaskIDs) != 2 {
		t.Fatalf("approve = %d %+v %+v", status, answer.TaskIDs, failure)
	}
	for _, task := range answer.TaskIDs {
		requested, rows := w.trackingOf(task)
		if !requested || w.laneOf(task) != mailer.PriorityBulk {
			t.Errorf("send %s: tracking_requested %v, lane %d", task, requested, w.laneOf(task))
		}
		for email, row := range rows {
			if email == elif.email && (row.sub != elif.sub || len(row.token) != tracking.RecipientTokenBytes) {
				t.Errorf("Elif's row %+v; want her account and a token", row)
			}
			if email == ayseKaya.Email && (row.token != nil || row.reason != tracking.SkipNoAccount) {
				t.Errorf("Ayşe's row %+v; want no_account", row)
			}
		}
	}

	// A request to a list that does not ask, submitted with operator
	// tracking on, is tracked once approved too.
	listRequest := w.submit("elif", w.listSend())
	status, answer, failure = w.act("fatih", listRequest.ID, "approve", nil)
	if status != fiber.StatusOK || answer.send() == uuid.Nil {
		t.Fatalf("approve the list request = %d %+v %+v", status, answer.TaskIDs, failure)
	}
	if requested, _ := w.trackingOf(answer.send()); !requested {
		t.Error("an approved list request is not tracked with operator tracking on")
	}
}

// A service's single mail says whether its recipient is tracked, not
// whether the send is: Elif is (true); Başka turned it off and Ayşe has no
// account (false), though the send is tracked for both.
func TestAServicesSingleMailSaysWhetherItsRecipientIsTracked(t *testing.T) {
	w, _ := systemTrackedWorld(t)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	service := uuid.NewString()
	for _, c := range []struct {
		email, reason string
		tracked       bool
	}{{elif.email, "", true}, {baska.email, tracking.SkipPreferenceOff, false}, {ayseKaya.Email, tracking.SkipNoAccount, false}} {
		body, _ := json.Marshal(w.singleSendTo(c.email, "Kişi", false))
		request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer(t, service, true, "skymail:access", "skymail:mails:send"))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		var created createdSend
		if response.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Tracking == nil {
			t.Fatalf("POST /v1/mail_tasks/single = %d %s", response.StatusCode, raw)
		}
		requested, row := w.onlyRow(created.ID)
		if *created.Tracking != c.tracked || !requested || row.reason != c.reason || (len(row.token) == tracking.RecipientTokenBytes) != c.tracked {
			t.Errorf("%s: answer %v, requested %v, row %+v; want answer %v, reason %q", c.email, *created.Tracking, requested, row, c.tracked, c.reason)
		}
	}
}

// Who opened a mail the system sends is read with the role only, even when
// sent_by names a person (KVKK notice s3, §4): the requester whose request
// sent the approvers their notice, and the approver whose decision sent the
// requester theirs, are refused its people. The requester still reads the
// people of her own send, approved and queued in the bulk lane.
func TestASystemMailsPeopleAreReadWithTheRoleOnly(t *testing.T) {
	w, _ := systemTrackedWorld(t)
	w.account(fatih, true, true)
	w.account(yusuf, true, true)
	request := w.submit("elif", w.listSend())
	status, answer, failure := w.act("fatih", request.ID, "approve", nil)
	if status != fiber.StatusOK || answer.send() == uuid.Nil {
		t.Fatalf("approve = %d %+v %+v", status, answer.TaskIDs, failure)
	}
	people := func(who string, task uuid.UUID) int {
		t.Helper()
		status, _ := w.call(who, fiber.MethodGet, "/v1/mail_tasks/"+task.String()+"/tracking/recipients", nil, nil)
		return status
	}
	notices := map[uuid.UUID]int{}
	for _, sent := range w.mail.sent {
		switch sent.templateID {
		case w.requested.ID, w.resolved.ID:
			notices[sent.templateID]++
			if requested, _ := w.onlyRow(sent.taskID); !requested || w.laneOf(sent.taskID) != mailer.PrioritySystem {
				t.Errorf("notice %s: tracking_requested %v, lane %d; want tracked in the system lane", sent.taskID, requested, w.laneOf(sent.taskID))
			}
			who := map[string]string{elif.sub: "elif", fatih.sub: "fatih"}[sent.sentBy]
			if who == "" {
				t.Fatalf("notice sent by %q, want Elif or Fatih", sent.sentBy)
			}
			if status := people(who, sent.taskID); status != fiber.StatusForbidden {
				t.Errorf("%s read the people of the notice their action sent: %d, want 403", who, status)
			}
		}
	}
	if notices[w.requested.ID] != 2 || notices[w.resolved.ID] != 1 {
		t.Fatalf("notices %v; want 2 requested and 1 resolved", notices)
	}
	if status := people("elif", answer.send()); status != fiber.StatusOK {
		t.Errorf("Elif reading the people of her own approved send: %d, want 200", status)
	}
}
