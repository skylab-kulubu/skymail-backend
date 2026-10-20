package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// Tracking in a mail sent to people one by one (Yusuf, 2026-10-20): the
// compose form's people (POST /mail_tasks/single) and a request for approval
// to people may ask for it, as a list send does. SkyMail knows these people
// by address; before the send's transaction it asks Keycloak for the one
// enabled account whose verified primary e-mail is the address, and then
// core, as for a group's members. No such account: no_account. Keycloak not
// answering: preference_unavailable, and the send goes on. A service's
// single mail — account and transaction mail — is never tracked, nor a
// System or sensitive template.

// setFreeBasic sets free.basic's System flag and content_sensitive (nil: the
// template says nothing).
func (w *approvalWorld) setFreeBasic(system bool, sensitive *bool) {
	w.t.Helper()
	if _, err := w.store.Conn.Exec(context.Background(), `UPDATE templates SET system = $2, content_sensitive = $3 WHERE id = $1`,
		w.freeBasic.ID, system, sensitive); err != nil {
		w.t.Fatal(err)
	}
}

// account gives p a Keycloak account under their address, as the exact
// e-mail search finds it.
func (w *approvalWorld) account(p approvalPerson, enabled, verified bool) {
	user := p.user()
	user.Enabled, user.EmailVerified = gocloak.BoolP(enabled), gocloak.BoolP(verified)
	if w.directory.users == nil {
		w.directory.users = map[string][]*gocloak.User{}
	}
	w.directory.users[strings.ToLower(p.email)] = []*gocloak.User{user}
}

// accountsWorld is a tracked world where Elif and Başka have verified
// accounts (core lets Elif be tracked, not Başka) and Ayşe has none;
// Keycloak counts the transactions open when it is asked.
func accountsWorld(t *testing.T, on bool) (*approvalWorld, *routePreferences) {
	w, prefs := trackedWorld(t, on)
	notSensitive := false
	w.setFreeBasic(false, &notSensitive)
	w.account(elif, true, true)
	w.account(baska, true, true)
	w.directory.store = w.store
	return w, prefs
}

// singleSendTo is free.basic to one address, as the compose form sends one
// of its people.
func (w *approvalWorld) singleSendTo(email, name string, tracked bool) map[string]any {
	send := w.listSend()
	delete(send, "mail_list_id")
	send["recipient_email"] = email
	send["recipient_full_name"] = name
	if tracked {
		send["tracking"] = true
	}
	return send
}

// laneOf is the lane every queue row of task is in.
func (w *approvalWorld) laneOf(task uuid.UUID) mailer.Priority {
	w.t.Helper()
	var lane int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT coalesce(max(priority), -1) FROM mail_queue WHERE task_id = $1`, task).Scan(&lane); err != nil {
		w.t.Fatal(err)
	}
	return mailer.Priority(lane)
}

// onlyRow is the one queue row of a single send.
func (w *approvalWorld) onlyRow(task uuid.UUID) (bool, sendRow) {
	w.t.Helper()
	requested, rows := w.trackingOf(task)
	if len(rows) != 1 {
		w.t.Fatalf("send %s has rows %+v, want one", task, rows)
	}
	for _, row := range rows {
		return requested, row
	}
	return requested, sendRow{}
}

type createdSend struct {
	ID       uuid.UUID `json:"id"`
	Tracking *bool     `json:"tracking"`
}

func (w *approvalWorld) sendSingle(send map[string]any) createdSend {
	w.t.Helper()
	var created createdSend
	status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks/single", send, &created)
	if status != fiber.StatusCreated || created.Tracking == nil {
		w.t.Fatalf("POST /v1/mail_tasks/single = %d %s", status, raw)
	}
	return created
}

// A person's single send takes tracking. On, it says tracking true and goes
// in the bulk lane; the recipient's account is the one Keycloak finds for
// the address, whatever its case, and core decides as for anyone: Elif is
// tracked, Başka turned it off, Ayşe has no account. Keycloak and core are
// asked with no transaction open. Off, the field is ignored and no one is
// asked. Without the field the mail goes in the system lane, untracked.
func TestAPersonsSingleSendTakesTracking(t *testing.T) {
	for _, on := range []bool{true, false} {
		w, prefs := accountsWorld(t, on)

		for _, c := range []struct {
			name, email, sub, reason string
			token                    bool
		}{
			{"Elif", strings.ToUpper(elif.email), elif.sub, "", true},
			{"Başka", baska.email, baska.sub, tracking.SkipPreferenceOff, false},
			{"Ayşe", ayseKaya.Email, "", tracking.SkipNoAccount, false},
		} {
			created := w.sendSingle(w.singleSendTo(c.email, c.name, true))
			requested, row := w.onlyRow(created.ID)
			if *created.Tracking != on || requested != on {
				t.Errorf("on=%v %s: answer %v, tracking_requested %v", on, c.name, *created.Tracking, requested)
			}
			if !on {
				if row.sub != "" || row.token != nil || row.reason != "" || w.laneOf(created.ID) != mailer.PrioritySystem {
					t.Errorf("off %s: row %+v in lane %d; want the untracked system lane", c.name, row, w.laneOf(created.ID))
				}
				continue
			}
			if row.sub != c.sub || (len(row.token) == tracking.RecipientTokenBytes) != c.token || row.reason != c.reason {
				t.Errorf("%s: row %+v; want sub %q, token %v, reason %q", c.name, row, c.sub, c.token, c.reason)
			}
			if w.laneOf(created.ID) != mailer.PriorityBulk {
				t.Errorf("%s: lane %d, want the bulk lane", c.name, w.laneOf(created.ID))
			}
		}
		if on {
			if w.directory.accountAsks != 3 || prefs.asked != 2 {
				t.Errorf("Keycloak asked %d times, core %d; want 3 and 2 (no one to ask core about for Ayşe)", w.directory.accountAsks, prefs.asked)
			}
			for _, open := range append(append([]int{}, w.directory.openTx...), prefs.openTx...) {
				if open != 0 {
					t.Errorf("asked with %d transactions open: Keycloak %v, core %v", open, w.directory.openTx, prefs.openTx)
					break
				}
			}
		} else if w.directory.accountAsks != 0 || prefs.asked != 0 {
			t.Errorf("off: Keycloak asked %d times, core %d", w.directory.accountAsks, prefs.asked)
		}

		// Without the field: the system lane, untracked, no one asked.
		asks := w.directory.accountAsks
		created := w.sendSingle(w.singleSendTo(elif.email, "Elif", false))
		if requested, row := w.onlyRow(created.ID); *created.Tracking || requested || row.sub != "" || row.reason != "" {
			t.Errorf("on=%v: a single send without tracking: %v %+v", on, requested, row)
		}
		if w.laneOf(created.ID) != mailer.PrioritySystem || w.directory.accountAsks != asks {
			t.Errorf("on=%v: a single send without tracking is in lane %d and asked Keycloak", on, w.laneOf(created.ID))
		}
	}
}

// Only an enabled account with the address verified is the recipient's.
func TestASingleSendsAccountMustBeEnabledAndVerified(t *testing.T) {
	w, prefs := accountsWorld(t, true)
	for _, c := range []struct {
		name              string
		enabled, verified bool
	}{{"unverified", true, false}, {"disabled", false, true}} {
		w.account(elif, c.enabled, c.verified)
		created := w.sendSingle(w.singleSendTo(elif.email, "Elif", true))
		if _, row := w.onlyRow(created.ID); row.sub != "" || row.token != nil || row.reason != tracking.SkipNoAccount {
			t.Errorf("%s: row %+v; want no_account", c.name, row)
		}
	}
	if prefs.asked != 0 {
		t.Errorf("core asked %d times about addresses with no account", prefs.asked)
	}
}

// Keycloak not answering: the recipient is not tracked
// (preference_unavailable), core is not asked, the mail goes out all the same.
func TestASingleSendGoesOnWhenKeycloakDoesNotAnswer(t *testing.T) {
	w, prefs := accountsWorld(t, true)
	w.directory.accountsErr = errors.New("keycloak down")
	created := w.sendSingle(w.singleSendTo(elif.email, "Elif", true))
	requested, row := w.onlyRow(created.ID)
	if !*created.Tracking || !requested || row.sub != "" || row.token != nil || row.reason != tracking.SkipPreferenceUnavailable {
		t.Errorf("answer %v, requested %v, row %+v; want tracked send, row preference_unavailable", *created.Tracking, requested, row)
	}
	if prefs.asked != 0 {
		t.Errorf("core asked %d times", prefs.asked)
	}
}

// A single send of a template that may never be tracked says tracking false
// and goes in the system lane, asking no one: a System template, a sensitive
// one, and — to one person — one that says nothing of its content, which
// single mail reads as sensitive.
func TestASingleSendThatCannotBeTrackedSaysSo(t *testing.T) {
	w, prefs := accountsWorld(t, true)
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
		created := w.sendSingle(w.singleSendTo(elif.email, "Elif", true))
		if requested, row := w.onlyRow(created.ID); *created.Tracking || requested || row.sub != "" || row.reason != "" {
			t.Errorf("%s: answer %v, the send says tracking %v, row %+v", c.name, *created.Tracking, requested, row)
		}
		if w.laneOf(created.ID) != mailer.PrioritySystem {
			t.Errorf("%s: the send is in lane %d, want the system lane", c.name, w.laneOf(created.ID))
		}
	}
	if prefs.asked != 0 || w.directory.accountAsks != 0 {
		t.Errorf("core asked %d times, Keycloak %d", prefs.asked, w.directory.accountAsks)
	}
}

// A request for approval to people takes tracking as one to a list does.
// Approved, each person's send asks for it; Keycloak (each address once)
// and core are asked before the request is locked. Elif is tracked, Ayşe has
// no account. A template that says nothing of its content is sensitive for
// people: the request says tracking false.
func TestARequestToPeopleTakesTracking(t *testing.T) {
	for _, on := range []bool{true, false} {
		w, prefs := accountsWorld(t, on)
		send := w.peopleSend(ayseKaya, approvalRecipient{Email: elif.email, FullName: "Elif Yıldız"})
		send["tracking"] = true
		var submitted map[string]any
		status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &submitted)
		if status != fiber.StatusCreated || submitted["tracking"] != on {
			t.Fatalf("on=%v: submit = %d %s, want tracking %v", on, status, raw, on)
		}
		if w.directory.accountAsks != 0 {
			t.Errorf("on=%v: Keycloak asked at submission", on)
		}
		id := uuid.MustParse(submitted["id"].(string))
		status, answer, failure := w.act("fatih", id, "approve", nil)
		if status != fiber.StatusOK || len(answer.TaskIDs) != 2 {
			t.Fatalf("on=%v: approve = %d %+v %+v", on, status, answer.TaskIDs, failure)
		}
		want := map[string]sendRow{ayseKaya.Email: {}, elif.email: {}}
		if on {
			want = map[string]sendRow{ayseKaya.Email: {reason: tracking.SkipNoAccount}, elif.email: {sub: elif.sub}}
		}
		for _, task := range answer.TaskIDs {
			requested, rows := w.trackingOf(task)
			if requested != on || len(rows) != 1 || w.laneOf(task) != mailer.PriorityBulk {
				t.Errorf("on=%v: send %s tracking_requested %v, rows %+v, lane %d", on, task, requested, rows, w.laneOf(task))
			}
			for email, row := range rows {
				tracked := len(row.token) == tracking.RecipientTokenBytes
				if row.sub != want[email].sub || row.reason != want[email].reason || tracked != (on && email == elif.email) {
					t.Errorf("on=%v: %s's row %+v, want %+v", on, email, row, want[email])
				}
			}
		}
		if on {
			if w.directory.accountAsks != 2 || prefs.asked != 1 {
				t.Errorf("Keycloak asked %d times, core %d; want 2 and 1", w.directory.accountAsks, prefs.asked)
			}
			for _, open := range append(append([]int{}, w.directory.openTx...), prefs.openTx...) {
				if open != 0 {
					t.Errorf("asked with a transaction open: Keycloak %v, core %v", w.directory.openTx, prefs.openTx)
					break
				}
			}
		} else if w.directory.accountAsks != 0 || prefs.asked != 0 {
			t.Errorf("off: Keycloak asked %d times, core %d", w.directory.accountAsks, prefs.asked)
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

// Keycloak not answering at approval: the people are not tracked
// (preference_unavailable), the request is sent all the same.
func TestARequestToPeopleGoesOnWhenKeycloakDoesNotAnswer(t *testing.T) {
	w, prefs := accountsWorld(t, true)
	w.directory.accountsErr = errors.New("keycloak down")
	send := w.peopleSend(ayseKaya, approvalRecipient{Email: elif.email, FullName: "Elif Yıldız"})
	send["tracking"] = true
	submitted := w.submit("elif", send)
	status, answer, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || len(answer.TaskIDs) != 2 {
		t.Fatalf("approve = %d %+v %+v", status, answer.TaskIDs, failure)
	}
	for _, task := range answer.TaskIDs {
		if requested, row := w.onlyRow(task); !requested || row.sub != "" || row.token != nil || row.reason != tracking.SkipPreferenceUnavailable {
			t.Errorf("send %s: %v %+v; want preference_unavailable", task, requested, row)
		}
	}
	if prefs.asked != 0 {
		t.Errorf("core asked %d times", prefs.asked)
	}
}

// singleSendWorld is POST /v1/mail_tasks/single through the chain production
// registers — userinfo, skymail:access, the role, the handler — with tracking
// on, so a service's token and a person's are told apart as Authenticate
// accepted them.
func singleSendWorld(t *testing.T) (*fiber.App, *approvalWorld) {
	t.Helper()
	w, _ := accountsWorld(t, true)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	return app, w
}

// A service's single mail is account or transaction mail: it is never
// tracked, though it asks, stays in the system lane, and Keycloak is not
// asked about its recipient. A person's token through the same chain gets it.
func TestAServicesSingleSendIsNeverTracked(t *testing.T) {
	app, w := singleSendWorld(t)
	for _, c := range []struct {
		name    string
		service bool
	}{{"service", true}, {"person", false}} {
		asks := w.directory.accountAsks
		body, _ := json.Marshal(w.singleSendTo(elif.email, "Elif", true))
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
		requested, row := w.onlyRow(created.ID)
		wantLane, wantSub := mailer.PriorityBulk, elif.sub
		if c.service {
			wantLane, wantSub = mailer.PrioritySystem, ""
		}
		if requested == c.service || w.laneOf(created.ID) != wantLane || row.sub != wantSub {
			t.Errorf("%s: tracking_requested %v in lane %d, row %+v; want %v in lane %d", c.name, requested, w.laneOf(created.ID), row, !c.service, wantLane)
		}
		if asked := w.directory.accountAsks - asks; asked != map[bool]int{true: 0, false: 1}[c.service] {
			t.Errorf("%s: Keycloak asked %d times", c.name, asked)
		}
	}
}
