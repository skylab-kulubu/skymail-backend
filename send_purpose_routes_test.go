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
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// A send's purpose and category (ADR 0072 decision 2; hub ticket
// abonelik-cikisi 03), through the routes production registers, on the real
// stores and queue: SKYMAIL_MAIL_PURPOSE on unless a test says otherwise.

// purposesOn is the mailer with SKYMAIL_MAIL_PURPOSE on and every lane off.
func purposesOn() mailer.Option { return mailer.WithSendLanes(mailer.SendLanes{Purpose: true}) }

// storedPurpose is what mail_tasks holds of a send's purpose.
type storedPurpose struct {
	purpose, category         *string
	transactional, commercial *bool
	tracked                   bool
}

func (w *approvalWorld) purposeOf(task uuid.UUID) storedPurpose {
	w.t.Helper()
	var p storedPurpose
	if err := w.store.Conn.QueryRow(context.Background(), `
		SELECT purpose, category, transactional_statement, commercial_content_absent, tracking_requested FROM mail_tasks WHERE id = $1`,
		task).Scan(&p.purpose, &p.category, &p.transactional, &p.commercial, &p.tracked); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// with is send with the fields given added.
func with(send map[string]any, fields map[string]any) map[string]any {
	out := make(map[string]any, len(send)+len(fields))
	for k, v := range send {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

var islemFields = map[string]any{"purpose": "islem", "transactional_statement": true}

// createdPurpose is the answer to a send, with its purpose.
type createdPurpose struct {
	ID       uuid.UUID `json:"id"`
	Tracking bool      `json:"tracking"`
	Purpose  *string   `json:"purpose"`
	Category *string   `json:"category"`
}

// What ADR 0072 refuses is refused, before anything is queued: islem to a
// list, a group, or as a Mail onayı request to a list; islem without the
// statement or with a category; duyuru without core's category; commercial
// content declared, whatever the purpose; a category without a purpose.
func TestThePurposeRulesRefuseASend(t *testing.T) {
	w := newFormSourceWorld(t, purposesOn())
	source := prepareSource(t, w, "source-sender", 2)
	group := w.listSend()
	group["mail_list_id"] = w.group
	single := w.listSend()
	delete(single, "mail_list_id")
	single["recipient_email"], single["recipient_full_name"] = "ayse@example.com", "Ayşe Kaya"
	for _, tc := range []struct {
		name, who, path string
		body            map[string]any
		code            string
	}{
		{"islem to a list", "yusuf", "/v1/mail_tasks", with(w.listSend(), islemFields), "mail_task.purpose_not_allowed"},
		{"islem to a Keycloak group", "yusuf", "/v1/mail_tasks", with(group, islemFields), "mail_task.purpose_not_allowed"},
		{"a Mail onayı request to a list as islem", "elif", "/v1/mail_approvals", with(w.listSend(), islemFields), "mail_task.purpose_not_allowed"},
		{"islem without the statement", "yusuf", "/v1/mail_tasks/single", with(single, map[string]any{"purpose": "islem"}), "mail_task.transactional_statement_required"},
		{"commercial content declared, duyuru", "yusuf", "/v1/mail_tasks", with(w.listSend(), map[string]any{"purpose": "duyuru", "category": "club:duyuru", "commercial_content_absent": false}), "mail_task.commercial_content_not_allowed"},
		{"commercial content declared, islem", "yusuf", "/v1/mail_tasks/single", with(single, map[string]any{"purpose": "islem", "transactional_statement": true, "commercial_content_absent": false}), "mail_task.commercial_content_not_allowed"},
		{"commercial content declared, a Forms source", "source-sender", "/v1/mail_tasks/form-source", with(sourceSend(w, source.ID), map[string]any{"purpose": "duyuru", "category": "club:haber", "commercial_content_absent": false}), "mail_task.commercial_content_not_allowed"},
		{"islem with a category", "yusuf", "/v1/mail_tasks/single", with(single, map[string]any{"purpose": "islem", "transactional_statement": true, "category": "club:duyuru"}), "mail_task.category_not_allowed"},
		{"a Forms source as islem without the statement", "source-sender", "/v1/mail_tasks/form-source", with(sourceSend(w, source.ID), map[string]any{"purpose": "islem", "transactional_statement": false}), "mail_task.transactional_statement_required"},
		{"duyuru without a category", "yusuf", "/v1/mail_tasks", with(w.listSend(), map[string]any{"purpose": "duyuru"}), "mail_task.category_required"},
		{"duyuru with a category core has not", "yusuf", "/v1/mail_tasks", with(w.listSend(), map[string]any{"purpose": "duyuru", "category": "club:bulten"}), "mail_task.category_invalid"},
		{"a category without a purpose", "yusuf", "/v1/mail_tasks", with(w.listSend(), map[string]any{"category": "club:haber"}), "mail_task.purpose_required"},
		{"a purpose SkyMail has not", "yusuf", "/v1/mail_tasks", with(w.listSend(), map[string]any{"purpose": "ticari", "category": "club:duyuru"}), "mail_task.purpose_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failure apiError
			status, raw := w.call(tc.who, http.MethodPost, tc.path, tc.body, &failure)
			if status != fiber.StatusBadRequest || failure.Code != tc.code {
				t.Errorf("= %d %s, want 400 %s", status, raw, tc.code)
			}
		})
	}
	var sends int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks`).Scan(&sends); err != nil || sends != 0 {
		t.Errorf("%d sends queued (%v), want none", sends, err)
	}
	var requests int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_approvals`).Scan(&requests); err != nil || requests != 0 {
		t.Errorf("%d requests written (%v), want none", requests, err)
	}
}

// A person's send whose body names no purpose (an older client) is duyuru,
// club:duyuru (ADR 0072 decision 2), in the bulk lane: a list, a group, a
// Forms source, and an untracked single mail too.
func TestAnOlderClientsSendGetsThePurposeItAlwaysHad(t *testing.T) {
	w := newFormSourceWorld(t, purposesOn())
	group := w.listSend()
	group["mail_list_id"] = w.group
	single := w.listSend()
	delete(single, "mail_list_id")
	single["recipient_email"], single["recipient_full_name"] = "ayse@example.com", "Ayşe Kaya"
	source := prepareSource(t, w, "source-sender", 2)
	for _, tc := range []struct {
		name, who, path string
		body            map[string]any
		purpose         string
		category        string
		lane            mailer.Priority
	}{
		{"a list", "yusuf", "/v1/mail_tasks", w.listSend(), "duyuru", "club:duyuru", mailer.PriorityBulk},
		{"a Keycloak group", "yusuf", "/v1/mail_tasks", group, "duyuru", "club:duyuru", mailer.PriorityBulk},
		{"a Forms source", "source-sender", "/v1/mail_tasks/form-source", sourceSend(w, source.ID), "duyuru", "club:duyuru", mailer.PriorityBulk},
		{"an untracked single mail", "yusuf", "/v1/mail_tasks/single", single, "duyuru", "club:duyuru", mailer.PriorityBulk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var created createdPurpose
			status, raw := w.call(tc.who, http.MethodPost, tc.path, tc.body, &created)
			if status != fiber.StatusCreated || str(created.Purpose) != tc.purpose || str(created.Category) != tc.category {
				t.Fatalf("= %d %s, want 201 %s %s", status, raw, tc.purpose, tc.category)
			}
			got := w.purposeOf(created.ID)
			if str(got.purpose) != tc.purpose || str(got.category) != tc.category || got.transactional != nil || got.commercial != nil || w.laneOf(created.ID) != tc.lane {
				t.Errorf("written %s %s %v %v in lane %d, want %s %s in lane %d", str(got.purpose), str(got.category), got.transactional, got.commercial, w.laneOf(created.ID), tc.purpose, tc.category, tc.lane)
			}
		})
	}
}

// What a sender chose is written and read back: islem to a person in the
// system lane with the statement; duyuru to a person, untracked, in the bulk
// lane with a team's category (lower-cased) and the commercial statement;
// islem to a Forms source in the bulk lane, whose exact retry answers the
// same; islem through Mail onayı to people, its sends islem. The send list,
// a send's page and the request say it.
func TestASendersPurposeIsWrittenAndReadBack(t *testing.T) {
	w := newFormSourceWorld(t, purposesOn())
	single := w.listSend()
	delete(single, "mail_list_id")
	single["recipient_email"], single["recipient_full_name"] = "ayse@example.com", "Ayşe Kaya"

	var islem createdPurpose
	if status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/single", with(single, islemFields), &islem); status != fiber.StatusCreated || str(islem.Purpose) != "islem" {
		t.Fatalf("islem single = %d %s", status, raw)
	}
	if got := w.purposeOf(islem.ID); str(got.purpose) != "islem" || got.transactional == nil || !*got.transactional || got.category != nil || w.laneOf(islem.ID) != mailer.PrioritySystem {
		t.Errorf("islem single written %+v in lane %d", got, w.laneOf(islem.ID))
	}

	var duyuru createdPurpose
	team := "team:6F1D2C3B-0A9E-4F8D-8C7B-6A5E4D3C2B1A:haber"
	if status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/single", with(single, map[string]any{"purpose": "duyuru", "category": team, "commercial_content_absent": true}), &duyuru); status != fiber.StatusCreated {
		t.Fatalf("duyuru single = %d %s", status, raw)
	}
	if got := w.purposeOf(duyuru.ID); str(got.purpose) != "duyuru" || str(got.category) != "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:haber" ||
		got.commercial == nil || !*got.commercial || got.tracked || w.laneOf(duyuru.ID) != mailer.PriorityBulk {
		t.Errorf("duyuru single written %s %s %v tracked %v in lane %d", str(got.purpose), str(got.category), got.commercial, got.tracked, w.laneOf(duyuru.ID))
	}

	source := prepareSource(t, w, "source-sender", 3)
	body := with(sourceSend(w, source.ID), islemFields)
	var fromSource createdPurpose
	if status, raw := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", body, &fromSource); status != fiber.StatusCreated || str(fromSource.Purpose) != "islem" {
		t.Fatalf("islem Forms source = %d %s", status, raw)
	}
	if got := w.purposeOf(fromSource.ID); str(got.purpose) != "islem" || got.tracked || w.laneOf(fromSource.ID) != mailer.PriorityBulk {
		t.Errorf("islem Forms source written %s tracked %v in lane %d, want islem untracked in the bulk lane", str(got.purpose), got.tracked, w.laneOf(fromSource.ID))
	}
	var again createdPurpose
	if status, raw := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", body, &again); status != fiber.StatusOK || again.ID != fromSource.ID || str(again.Purpose) != "islem" {
		t.Errorf("exact retry = %d %s, want 200, the same send, islem", status, raw)
	}
	if status, raw := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", sourceSend(w, source.ID), nil); status != fiber.StatusConflict {
		t.Errorf("the same source as duyuru = %d %s, want 409: not the request it consumed", status, raw)
	}

	var item handlers.MailTaskItem
	if status, raw := w.call("baska", http.MethodGet, "/v1/mail_tasks/"+fromSource.ID.String(), nil, &item); status != fiber.StatusOK ||
		str(item.Purpose) != "islem" || item.TransactionalStatement == nil || !*item.TransactionalStatement || item.Category != nil {
		t.Errorf("GET the send = %d %s", status, raw)
	}
	var page []handlers.MailTaskItem
	if status, _ := w.call("baska", http.MethodGet, "/v1/mail_tasks?_start=0&_end=10", nil, &page); status != fiber.StatusOK || len(page) != 3 {
		t.Fatalf("the send list = %d, %d sends", status, len(page))
	}
	for _, send := range page {
		if send.Purpose == nil {
			t.Errorf("send %s listed without its purpose", send.ID)
		}
	}

	people := w.listSend()
	delete(people, "mail_list_id")
	people["recipients"] = []map[string]any{{"email": "ayse@example.com", "full_name": "Ayşe Kaya"}, {"email": "mehmet@example.com"}}
	var submitted struct {
		ID uuid.UUID `json:"id"`
		handlers.SendPurposeFields
	}
	if status, raw := w.call("okur", http.MethodPost, "/v1/mail_approvals", with(people, islemFields), &submitted); status != fiber.StatusCreated ||
		str(submitted.Purpose) != "islem" || submitted.TransactionalStatement == nil {
		t.Fatalf("submit islem to people = %d %s", status, raw)
	}
	status, answer, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || len(answer.TaskIDs) != 2 {
		t.Fatalf("approve = %d %+v %+v", status, answer.TaskIDs, failure)
	}
	for _, task := range answer.TaskIDs {
		if got := w.purposeOf(task); str(got.purpose) != "islem" || got.transactional == nil || got.tracked || w.laneOf(task) != mailer.PriorityBulk {
			t.Errorf("approved send %s written %s tracked %v in lane %d", task, str(got.purpose), got.tracked, w.laneOf(task))
		}
	}
}

// With every person's send tracked (SKYMAIL_TRACKING_OPERATOR; Yusuf: every
// mail is tracked), an islem send is tracked as any operator's send: a Forms
// source's respondents looked up and given tokens as for duyuru, an islem
// request approved tracked, an islem single mail tracked in the bulk lane,
// and one that asks for tracking itself is not refused.
func TestAnIslemSendIsTrackedLikeAnyOperatorSend(t *testing.T) {
	prefs := &routePreferences{}
	accounts := &laterAccounts{}
	w := newFormSourceWorld(t, purposesOn(), mailer.WithTracking(mailer.Tracking{
		Config: tracking.Config{Mode: tracking.ModeOn, Operator: tracking.ModeOn,
			BaseURL: "https://api.example.test/api/skymail", CoreURL: "http://core.invalid"},
		Preferences: prefs,
		Accounts:    accounts,
	}))
	prefs.store = w.store
	accounts.directory = w.directory
	notSensitive := false
	w.setFreeBasic(false, &notSensitive)
	w.account(approvalPerson{sub: elif.sub, name: "Source Recipient 0", email: "source-0@example.invalid"}, true, true)
	w.account(elif, true, true)
	w.directory.store = w.store

	source := prepareSource(t, w, "source-sender", 3)
	var created createdPurpose
	if status, raw := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", with(sourceSend(w, source.ID), islemFields), &created); status != fiber.StatusCreated || !created.Tracking || str(created.Purpose) != "islem" {
		t.Fatalf("islem Forms source = %d %s; want 201, tracked islem", status, raw)
	}
	checkFormSourceRows(t, w, created.ID, 3)
	if w.laneOf(created.ID) != mailer.PriorityBulk {
		t.Errorf("islem Forms source in lane %d", w.laneOf(created.ID))
	}

	approvalSource := prepareSource(t, w, "okur", 2)
	var submitted map[string]any
	if status, raw := w.call("okur", http.MethodPost, "/v1/mail_approvals", with(sourceSend(w, approvalSource.ID), islemFields), &submitted); status != fiber.StatusCreated || submitted["tracking"] != true || submitted["purpose"] != "islem" {
		t.Fatalf("submit islem Forms source = %d %s", status, raw)
	}
	status, answer, failure := w.act("fatih", uuid.MustParse(submitted["id"].(string)), "approve", nil)
	if status != fiber.StatusOK || answer.send() == uuid.Nil {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if got := w.purposeOf(answer.send()); str(got.purpose) != "islem" || !got.tracked {
		t.Errorf("approved islem send written %s tracked %v, want tracked islem", str(got.purpose), got.tracked)
	}

	single := w.listSend()
	delete(single, "mail_list_id")
	single["recipient_email"], single["recipient_full_name"] = elif.email, elif.name
	for _, body := range []map[string]any{with(single, islemFields), with(with(single, islemFields), map[string]any{"tracking": true})} {
		var one createdPurpose
		if status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/single", body, &one); status != fiber.StatusCreated || !one.Tracking || str(one.Purpose) != "islem" {
			t.Fatalf("islem single = %d %s; want 201, tracked islem", status, raw)
		}
		if got := w.purposeOf(one.ID); str(got.purpose) != "islem" || !got.tracked || w.laneOf(one.ID) != mailer.PriorityBulk {
			t.Errorf("islem single written %s tracked %v in lane %d", str(got.purpose), got.tracked, w.laneOf(one.ID))
		}
	}
}

// A service's single mail (account mail, a Forms confirmation) is islem,
// whatever its body says, in the system lane; a person's through the same
// chain may be duyuru.
func TestAServicesSingleMailIsAlwaysIslem(t *testing.T) {
	w := newApprovalWorldWithPool(t, 0, purposesOn())
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	send := w.listSend()
	delete(send, "mail_list_id")
	send["recipient_email"], send["recipient_full_name"] = "ayse@example.com", "Ayşe Kaya"
	send = with(send, map[string]any{"purpose": "duyuru", "category": "club:haber"})
	for _, c := range []struct {
		name    string
		service bool
		purpose string
		lane    mailer.Priority
	}{{"service", true, "islem", mailer.PrioritySystem}, {"person", false, "duyuru", mailer.PriorityBulk}} {
		body, _ := json.Marshal(send)
		request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer(t, uuid.NewString(), c.service, "skymail:access", "skymail:mails:send"))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		var created createdPurpose
		if response.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil || str(created.Purpose) != c.purpose {
			t.Fatalf("%s: = %d %s, want %s", c.name, response.StatusCode, raw, c.purpose)
		}
		if got := w.purposeOf(created.ID); str(got.purpose) != c.purpose || w.laneOf(created.ID) != c.lane || (c.service && (got.category != nil || got.transactional != nil)) {
			t.Errorf("%s: written %s %s in lane %d", c.name, str(got.purpose), str(got.category), w.laneOf(created.ID))
		}
	}
}

// SKYMAIL_MAIL_PURPOSE off (the default): the fields are ignored, nothing is
// written, a list's send goes as before; the tracking mode and the summary
// say whether purposes are on.
func TestPurposesOffIgnoreTheFields(t *testing.T) {
	off := newFormSourceWorld(t)
	var created createdPurpose
	if status, raw := off.call("yusuf", http.MethodPost, "/v1/mail_tasks", with(off.listSend(), islemFields), &created); status != fiber.StatusCreated || created.Purpose != nil {
		t.Fatalf("islem to a list, purposes off = %d %s; want 201, no purpose", status, raw)
	}
	if got := off.purposeOf(created.ID); got.purpose != nil || got.category != nil || got.transactional != nil || off.laneOf(created.ID) != mailer.PriorityBulk {
		t.Errorf("written %+v", got)
	}
	on := newFormSourceWorld(t, purposesOn())
	for _, c := range []struct {
		w    *approvalWorld
		want bool
	}{{off, false}, {on, true}} {
		var mode handlers.TrackingMode
		if status, raw := c.w.call("okur", http.MethodGet, "/v1/mail_tasks/tracking-mode", nil, &mode); status != fiber.StatusOK || mode.PurposeAvailable != c.want {
			t.Errorf("tracking mode = %d %s, want purpose_available %v", status, raw, c.want)
		}
		var summary handlers.SendSummary
		if status, raw := c.w.call("baska", http.MethodGet, "/v1/mail_tasks/summary", nil, &summary); status != fiber.StatusOK || summary.PurposeAvailable != c.want {
			t.Errorf("summary = %d %s, want purpose_available %v", status, raw, c.want)
		}
	}
}

// A request to a list holding islem (an older image's resubmit rewrites the
// audience and leaves the purpose) is queued as duyuru, club:duyuru.
func TestAnApprovedRequestToAListIsNeverIslem(t *testing.T) {
	w := newFormSourceWorld(t, purposesOn())
	people := w.listSend()
	delete(people, "mail_list_id")
	people["recipients"] = []map[string]any{{"email": "ayse@example.com", "full_name": "Ayşe Kaya"}}
	var submitted struct {
		ID uuid.UUID `json:"id"`
	}
	if status, raw := w.call("okur", http.MethodPost, "/v1/mail_approvals", with(people, islemFields), &submitted); status != fiber.StatusCreated {
		t.Fatalf("submit = %d %s", status, raw)
	}
	if _, err := w.store.Conn.Exec(context.Background(), `
		BEGIN;
		DELETE FROM mail_approval_recipients WHERE approval_id = '`+submitted.ID.String()+`';
		UPDATE mail_approvals SET mail_list_id = '`+w.list.ID.String()+`' WHERE id = '`+submitted.ID.String()+`';
		COMMIT;`); err != nil {
		t.Fatal(err)
	}
	status, answer, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || answer.send() == uuid.Nil {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if got := w.purposeOf(answer.send()); str(got.purpose) != "duyuru" || str(got.category) != "club:duyuru" || got.transactional != nil {
		t.Errorf("approved list send written %s %s %v, want duyuru club:duyuru", str(got.purpose), str(got.category), got.transactional)
	}
}

// SKYMAIL_PROMOTIONAL on: a duyuru send may say it is promotional, and is
// written so (the dispatcher then sends it only to those who consented,
// internal/mailer/consent_gate.go); an islem send still may not; the
// tracking mode says so.
func TestAPromotionalSendIsWrittenWhileTheSwitchIsOn(t *testing.T) {
	w := newFormSourceWorld(t, purposesOn(), mailer.WithConsentGate(mailer.ConsentGate{Promotional: true}))
	var created createdPurpose
	promotional := map[string]any{"purpose": "duyuru", "category": "club:duyuru", "commercial_content_absent": false}
	if status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks", with(w.listSend(), promotional), &created); status != fiber.StatusCreated {
		t.Fatalf("a promotional list send = %d %s, want 201", status, raw)
	}
	if got := w.purposeOf(created.ID); str(got.purpose) != "duyuru" || got.commercial == nil || *got.commercial {
		t.Errorf("written %s, commercial_content_absent %v; want duyuru, false", str(got.purpose), got.commercial)
	}
	single := w.listSend()
	delete(single, "mail_list_id")
	single["recipient_email"], single["recipient_full_name"] = "ayse@example.com", "Ayşe Kaya"
	var failure apiError
	if status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/single",
		with(single, map[string]any{"purpose": "islem", "transactional_statement": true, "commercial_content_absent": false}), &failure); status != fiber.StatusBadRequest || failure.Code != "mail_task.commercial_content_not_allowed" {
		t.Errorf("a promotional islem = %d %s, want 400 mail_task.commercial_content_not_allowed", status, raw)
	}
	var mode handlers.TrackingMode
	if status, raw := w.call("okur", http.MethodGet, "/v1/mail_tasks/tracking-mode", nil, &mode); status != fiber.StatusOK || !mode.PromotionalAvailable {
		t.Errorf("tracking mode = %d %s, want promotional_available", status, raw)
	}
}
