package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// routePreferences is core's answer in the route tests: Elif may be tracked,
// Başka turned it off. Each call counts the sessions sitting in an open
// transaction while core is asked: an approval must ask before it locks the
// request.
type routePreferences struct {
	mu     sync.Mutex
	asked  int
	openTx []int
	store  *database.Store
}

func (p *routePreferences) Resolve(ctx context.Context, subs []uuid.UUID) (map[uuid.UUID]tracking.Decision, error) {
	open := 0
	if p.store != nil {
		if err := p.store.Conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	p.asked++
	p.openTx = append(p.openTx, open)
	p.mu.Unlock()
	out := map[uuid.UUID]tracking.Decision{}
	for _, sub := range subs {
		switch sub.String() {
		case elif.sub:
			out[sub] = tracking.Decision{Allowed: true}
		default:
			out[sub] = tracking.Decision{Reason: tracking.SkipPreferenceOff}
		}
	}
	return out, nil
}

func trackedWorld(t *testing.T, on bool) (*approvalWorld, *routePreferences) {
	t.Helper()
	prefs := &routePreferences{}
	mode := tracking.ModeOff
	if on {
		mode = tracking.ModeOn
	}
	w := newApprovalWorldWithPool(t, 0, mailer.WithTracking(mailer.Tracking{
		Config:      tracking.Config{Mode: mode, BaseURL: "https://api.example.test/api/skymail", CoreURL: "http://core.invalid"},
		Preferences: prefs,
	}))
	prefs.store = w.store
	return w, prefs
}

type sendRow struct {
	sub    string
	token  []byte
	reason string
}

func (w *approvalWorld) trackingOf(task uuid.UUID) (bool, map[string]sendRow) {
	w.t.Helper()
	var requested bool
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT tracking_requested FROM mail_tasks WHERE id = $1`, task).Scan(&requested); err != nil {
		w.t.Fatal(err)
	}
	rows, err := w.store.Conn.Query(context.Background(), `
		SELECT recipient_email, coalesce(recipient_sub::text, ''), tracking_token, coalesce(tracking_skip_reason, '')
		FROM mail_queue WHERE task_id = $1`, task)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sendRow{}
	for rows.Next() {
		var email string
		var r sendRow
		if err := rows.Scan(&email, &r.sub, &r.token, &r.reason); err != nil {
			w.t.Fatal(err)
		}
		out[email] = r
	}
	return requested, out
}

// POST /v1/mail_tasks takes tracking. On, a group send tracks whom core
// allows and says tracking true; off, the field is ignored — not refused —
// and the answer says tracking false. The send list says the same.
func TestASendTakesTrackingAndSaysWhetherItGotIt(t *testing.T) {
	for _, on := range []bool{true, false} {
		w, prefs := trackedWorld(t, on)
		var created struct {
			ID       uuid.UUID `json:"id"`
			Tracking *bool     `json:"tracking"`
		}
		send := w.listSend()
		send["mail_list_id"] = w.group
		send["tracking"] = true
		status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks", send, &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking != on {
			t.Fatalf("on=%v: POST /v1/mail_tasks = %d %s, want 201 with tracking %v", on, status, raw, on)
		}
		requested, rows := w.trackingOf(created.ID)
		if requested != on {
			t.Errorf("on=%v: tracking_requested %v", on, requested)
		}
		if on {
			if len(rows[elif.email].token) != tracking.RecipientTokenBytes || rows[baska.email].reason != tracking.SkipPreferenceOff {
				t.Errorf("tracked group send rows = %+v", rows)
			}
		} else {
			for email, row := range rows {
				if row.token != nil || row.reason != "" {
					t.Errorf("off: %s %+v", email, row)
				}
			}
			if prefs.asked != 0 {
				t.Errorf("off: core asked %d times", prefs.asked)
			}
		}

		var detail map[string]any
		if status, raw := w.call("baska", fiber.MethodGet, "/v1/mail_tasks/"+created.ID.String(), nil, &detail); status != fiber.StatusOK || detail["tracking"] != on {
			t.Errorf("on=%v: GET the send = %d %s, want tracking %v", on, status, raw, on)
		}

		// Without the field a send does not ask.
		delete(send, "tracking")
		status, raw = w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks", send, &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking {
			t.Fatalf("on=%v: a send without tracking = %d %s", on, status, raw)
		}
		if requested, _ := w.trackingOf(created.ID); requested {
			t.Errorf("on=%v: a send without tracking asked for it", on)
		}
	}
}

// A request for approval keeps the submitter's tracking box, and its approved
// send asks for it; off, the box is kept false. A resubmission says it anew.
func TestAnApprovedSendKeepsTheSubmittersTracking(t *testing.T) {
	for _, on := range []bool{true, false} {
		w, prefs := trackedWorld(t, on)
		send := w.listSend()
		send["mail_list_id"] = w.group
		send["tracking"] = true
		var submitted map[string]any
		status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &submitted)
		if status != fiber.StatusCreated || submitted["tracking"] != on {
			t.Fatalf("on=%v: submit = %d %s, want tracking %v", on, status, raw, on)
		}
		id := uuid.MustParse(submitted["id"].(string))
		if status, _, failure := w.act("fatih", id, "approve", nil); status != fiber.StatusOK {
			t.Fatalf("on=%v: approve = %d %+v", on, status, failure)
		}
		sent := w.mail.of(w.freeBasic.ID)
		if len(sent) != 1 {
			t.Fatalf("on=%v: sends %+v", on, sent)
		}
		requested, rows := w.trackingOf(sent[0].taskID)
		if requested != on {
			t.Errorf("on=%v: the approved send's tracking_requested %v", on, requested)
		}
		if on && (len(rows[elif.email].token) != tracking.RecipientTokenBytes || rows[baska.email].reason != tracking.SkipPreferenceOff) {
			t.Errorf("the approved send's rows = %+v, want Elif tracked and Başka not", rows)
		}
		// Core was asked once, for the approval, before the request was
		// locked: no transaction open while waiting for it.
		if on && (prefs.asked != 1 || prefs.openTx[0] != 0) {
			t.Errorf("core asked %d times with %v transactions open; want once, with none", prefs.asked, prefs.openTx)
		}
		if !on && prefs.asked != 0 {
			t.Errorf("off: core asked %d times", prefs.asked)
		}
	}

	w, _ := trackedWorld(t, true)
	send := w.listSend()
	send["mail_list_id"] = w.group
	send["tracking"] = true
	first := w.submit("elif", send)
	if status, _, failure := w.act("fatih", first.ID, "reject", map[string]any{"reason": "Tarih yanlış."}); status != fiber.StatusOK {
		t.Fatalf("reject = %d %+v", status, failure)
	}
	delete(send, "tracking")
	var resubmitted map[string]any
	status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals/"+first.ID.String()+"/resubmit", send, &resubmitted)
	if status != fiber.StatusOK || resubmitted["tracking"] != false {
		t.Fatalf("resubmit without the box = %d %s, want tracking false", status, raw)
	}
	if status, _, failure := w.act("fatih", first.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	sent := w.mail.of(w.freeBasic.ID)
	if requested, _ := w.trackingOf(sent[len(sent)-1].taskID); requested {
		t.Error("the resubmitted request's send asked for tracking")
	}
}

// A send that can never be tracked — a System template, or one whose content
// is sensitive — says so: tracking false, though asked for with the flag on,
// in the answer, the send and a request for approval alike.
func TestASendThatCannotBeTrackedSaysSo(t *testing.T) {
	w, prefs := trackedWorld(t, true)
	ctx := context.Background()
	for _, change := range []string{
		`UPDATE templates SET system = true WHERE id = $1`,
		`UPDATE templates SET system = false, content_sensitive = true WHERE id = $1`,
	} {
		if _, err := w.store.Conn.Exec(ctx, change, w.freeBasic.ID); err != nil {
			t.Fatal(err)
		}
		send := w.listSend()
		send["mail_list_id"] = w.group
		send["tracking"] = true
		var created struct {
			ID       uuid.UUID `json:"id"`
			Tracking *bool     `json:"tracking"`
		}
		status, raw := w.call("yusuf", fiber.MethodPost, "/v1/mail_tasks", send, &created)
		if status != fiber.StatusCreated || created.Tracking == nil || *created.Tracking {
			t.Fatalf("%s: POST /v1/mail_tasks = %d %s, want tracking false", change, status, raw)
		}
		if requested, rows := w.trackingOf(created.ID); requested || len(rows[elif.email].token) != 0 {
			t.Errorf("%s: the send says tracking %v, rows %+v", change, requested, rows)
		}
		var submitted map[string]any
		if status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &submitted); status != fiber.StatusCreated || submitted["tracking"] != false {
			t.Errorf("%s: submit = %d %s, want tracking false", change, status, raw)
		}
	}
	if prefs.asked != 0 {
		t.Errorf("core asked %d times about sends that cannot be tracked", prefs.asked)
	}
}

// The summary says whether a send may ask for tracking.
func TestTheSummarySaysWhetherTrackingIsAvailable(t *testing.T) {
	for _, on := range []bool{true, false} {
		w, _ := trackedWorld(t, on)
		var summary map[string]json.RawMessage
		status, raw := w.call("baska", fiber.MethodGet, "/v1/mail_tasks/summary", nil, &summary)
		if status != fiber.StatusOK || string(summary["tracking_available"]) != map[bool]string{true: "true", false: "false"}[on] {
			t.Fatalf("on=%v: summary = %d %s", on, status, raw)
		}
	}
}
