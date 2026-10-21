package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// System tracking through the routes (SKYMAIL_TRACKING_SYSTEM, Yusuf
// 2026-10-21): with it on, the mail services send — a service's single mail
// (Keycloak, core, Forms), core's notifications, SkyMail's own approval
// notices — is tracked in its own lane; a person's send is as before. Off,
// nothing changes (TestAServicesSingleSendIsNeverTracked).

// systemTrackedWorld is accountsWorld with SKYMAIL_TRACKING_SYSTEM on.
func systemTrackedWorld(t *testing.T) (*approvalWorld, *routePreferences) {
	t.Helper()
	prefs := &routePreferences{}
	accounts := &laterAccounts{}
	w := newApprovalWorldWithPool(t, 0, mailer.WithTracking(mailer.Tracking{
		Config: tracking.Config{Mode: tracking.ModeOn, System: tracking.ModeOn,
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

func (w *approvalWorld) linkCount(task uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_task_links WHERE task_id = $1`, task).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// A service's single mail, with system tracking on, stays in the system lane
// and is tracked: the answer says so, the recipient's account is found by
// address (Keycloak and core asked with no transaction open) and core
// decides. It is tracked though it did not ask, and though its template is a
// System or sensitive one. A link in its body that the template does not
// write out (free.basic's body comes in a variable) is not rewritten. A
// person's send is as it was: untracked unless it asks, and then in the bulk
// lane. Who opened a service's mail is read with the tracking role only.
func TestAServicesSingleSendIsTrackedWithSystemTracking(t *testing.T) {
	w, prefs := systemTrackedWorld(t)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	serviceSub := uuid.NewString()
	post := func(service, asks bool) createdSend {
		t.Helper()
		send := w.singleSendTo(elif.email, "Elif", asks)
		send["body_variables"].(map[string]any)["BodyHtml"] = `<p><a href="https://forms.example.test/r/duzenle?token=tek-kullanimlik">Düzenle</a></p>`
		body, _ := json.Marshal(send)
		request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
		sub := uuid.NewString()
		if service {
			sub = serviceSub
		}
		request.Header.Set("Authorization", "Bearer "+bearer(t, sub, service, "skymail:access", "skymail:mails:send"))
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
		return created
	}

	var lastService uuid.UUID
	sensitive := true
	for _, c := range []struct {
		name      string
		system    bool
		sensitive *bool
	}{{"may be shown", false, nil}, {"System", true, nil}, {"sensitive", false, &sensitive}} {
		if c.system || c.sensitive != nil {
			w.setFreeBasic(c.system, c.sensitive)
		}
		for _, asks := range []bool{false, true} {
			asksBefore, coreBefore := w.directory.accountAsks, prefs.asked
			created := post(true, asks)
			lastService = created.ID
			requested, row := w.onlyRow(created.ID)
			if !*created.Tracking || !requested || w.laneOf(created.ID) != mailer.PrioritySystem {
				t.Errorf("%s, asks %v: answer %v, tracking_requested %v, lane %d; want tracked in the system lane",
					c.name, asks, *created.Tracking, requested, w.laneOf(created.ID))
			}
			if row.sub != elif.sub || len(row.token) != tracking.RecipientTokenBytes || row.reason != "" {
				t.Errorf("%s: row %+v; want Elif's account and a token", c.name, row)
			}
			if n := w.linkCount(created.ID); n != 0 {
				t.Errorf("%s: %d links registered; the body's link is a variable's", c.name, n)
			}
			if w.directory.accountAsks-asksBefore != 1 || prefs.asked-coreBefore != 1 {
				t.Errorf("%s: Keycloak asked %d times, core %d; want once each", c.name, w.directory.accountAsks-asksBefore, prefs.asked-coreBefore)
			}
		}
	}
	for _, open := range append(append([]int{}, w.directory.openTx...), prefs.openTx...) {
		if open != 0 {
			t.Fatalf("asked with a transaction open: Keycloak %v, core %v", w.directory.openTx, prefs.openTx)
		}
	}

	// Who opened a service's mail is read with the role only: the service,
	// though sent_by is its own, is no sender a person's report is for.
	read := func(sub string, service bool, roles ...string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/mail_tasks/"+lastService.String()+"/tracking/recipients", nil)
		request.Header.Set("Authorization", "Bearer "+bearer(t, sub, service, roles...))
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	if status := read(serviceSub, true, "skymail:access", "skymail:mails:send"); status != fiber.StatusForbidden {
		t.Errorf("the service read the people of the mail it sent: %d", status)
	}
	if status := read(uuid.NewString(), false, "skymail:access", handlers.TrackingReadRole); status != fiber.StatusOK {
		t.Errorf("a person with %s: %d", handlers.TrackingReadRole, status)
	}

	notSensitive := false
	w.setFreeBasic(false, &notSensitive)
	created := post(false, false)
	if requested, row := w.onlyRow(created.ID); *created.Tracking || requested || row.token != nil || w.laneOf(created.ID) != mailer.PrioritySystem {
		t.Errorf("a person's send that does not ask: answer %v, requested %v, row %+v, lane %d", *created.Tracking, requested, row, w.laneOf(created.ID))
	}
	created = post(false, true)
	if requested, row := w.onlyRow(created.ID); !*created.Tracking || !requested || len(row.token) == 0 || w.laneOf(created.ID) != mailer.PriorityBulk {
		t.Errorf("a person's send that asks: answer %v, requested %v, row %+v, lane %d", *created.Tracking, requested, row, w.laneOf(created.ID))
	}
}

// SkyMail's own notice to the approvers, with system tracking on, is
// tracked as a service's mail: each approver's account found by address,
// core decides (it says preference_off for both here), in the system lane.
// Keycloak and core are asked about every approver at once, before the
// first notice is queued (core once, not once per approver), with no
// transaction open.
func TestApprovalNoticesAreTrackedWithSystemTracking(t *testing.T) {
	w, prefs := systemTrackedWorld(t)
	w.account(fatih, true, true)
	w.account(yusuf, true, true)
	w.submit("elif", w.listSend())

	notices := 0
	for _, sent := range w.mail.sent {
		if sent.templateID != w.requested.ID {
			continue
		}
		notices++
		requested, row := w.onlyRow(sent.taskID)
		if !requested || w.laneOf(sent.taskID) != mailer.PrioritySystem || row.sub == "" || row.reason != tracking.SkipPreferenceOff {
			t.Errorf("notice to %v: tracking_requested %v, lane %d, row %+v; want tracked, preference_off", sent.recipients, requested, w.laneOf(sent.taskID), row)
		}
	}
	if notices != 2 || prefs.asked != 1 || w.directory.accountAsks != 2 {
		t.Fatalf("%d notices, core asked %d times, Keycloak %d; want 2, 1 and 2", notices, prefs.asked, w.directory.accountAsks)
	}
	for _, open := range append(append([]int{}, w.directory.openTx...), prefs.openTx...) {
		if open != 0 {
			t.Fatalf("asked with a transaction open: Keycloak %v, core %v", w.directory.openTx, prefs.openTx)
		}
	}
}

// notificationPreferences is core letting Ayşe be tracked, not Mehmet.
type notificationPreferences struct {
	mu    sync.Mutex
	asked int
}

func (p *notificationPreferences) Resolve(_ context.Context, subs []uuid.UUID) (map[uuid.UUID]tracking.Decision, error) {
	p.mu.Lock()
	p.asked++
	p.mu.Unlock()
	out := map[uuid.UUID]tracking.Decision{}
	for _, sub := range subs {
		out[sub] = tracking.Decision{Reason: tracking.SkipPreferenceOff}
		if sub == personAyse {
			out[sub] = tracking.Decision{Allowed: true}
		}
	}
	return out, nil
}

// core's notification, with system tracking on, is tracked in its type's
// lane: core is asked once, before the transaction, about the people it
// names. Off (the default), it is not.
func TestNotificationsAreTrackedWithSystemTracking(t *testing.T) {
	for _, system := range []tracking.Mode{tracking.ModeOn, tracking.ModeOff} {
		prefs := &notificationPreferences{}
		w := newNotificationWorld(t, mailer.WithTracking(mailer.Tracking{
			Config: tracking.Config{Mode: tracking.ModeOn, System: system,
				BaseURL: "https://api.example.test/api/skymail", CoreURL: "http://core.invalid"},
			Preferences: prefs,
		}))
		for _, c := range []struct {
			kind string
			lane mailer.Priority
		}{{"core.certificate", mailer.PrioritySystem}, {"event.cancelled", mailer.PriorityBulk}} {
			s := w.post(bearer(t, coreService, true, coreRoles...), c.kind+"-"+string(system), notification(c.kind,
				recipient(personAyse, "", "email"), recipient(personMehmet, "", "email")))
			email := s.result.Channels["email"]
			if s.status != fiber.StatusCreated || email.TaskID == nil || email.Queued != 2 {
				t.Fatalf("%s: %d %s", c.kind, s.status, s.raw)
			}
			var requested bool
			if err := w.store.Conn.QueryRow(context.Background(), `SELECT tracking_requested FROM mail_tasks WHERE id = $1`, *email.TaskID).Scan(&requested); err != nil {
				t.Fatal(err)
			}
			rows, err := w.store.Conn.Query(context.Background(),
				`SELECT recipient_email, tracking_token IS NOT NULL, coalesce(tracking_skip_reason, ''), priority FROM mail_queue WHERE task_id = $1`, *email.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for rows.Next() {
				var address, reason string
				var tracked bool
				var lane int16
				if err := rows.Scan(&address, &tracked, &reason, &lane); err != nil {
					t.Fatal(err)
				}
				if lane != int16(c.lane) {
					t.Errorf("%s: %s in lane %d, want %d", c.kind, address, lane, c.lane)
				}
				if tracked {
					reason = "tracked"
				}
				got[address] = reason
			}
			rows.Close()
			want := map[string]string{"ayse@example.com": "tracked", "mehmet@example.com": tracking.SkipPreferenceOff}
			if system == tracking.ModeOff {
				want = map[string]string{"ayse@example.com": "", "mehmet@example.com": ""}
			}
			if requested != (system == tracking.ModeOn) || len(got) != 2 || got["ayse@example.com"] != want["ayse@example.com"] || got["mehmet@example.com"] != want["mehmet@example.com"] {
				t.Errorf("system %s, %s: tracking_requested %v, rows %v; want %v", system, c.kind, requested, got, want)
			}
		}
		if wantAsked := map[tracking.Mode]int{tracking.ModeOn: 2, tracking.ModeOff: 0}[system]; prefs.asked != wantAsked {
			t.Errorf("system %s: core asked %d times, want %d", system, prefs.asked, wantAsked)
		}
	}
}
