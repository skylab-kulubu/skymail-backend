package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/consent"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// The bulk preference check (ADR 0072 decision 3; hub ticket
// abonelik-cikisi 09), through the routes production registers, on the real
// stores and queue.

// routeVerdicts is core's recipients:resolve: each address's reason ("" is
// allowed; an address it does not know has no account).
type routeVerdicts struct {
	mu      sync.Mutex
	reasons map[string]string
	unknown bool
	down    bool
	asked   int
}

func (f *routeVerdicts) Resolve(_ context.Context, _ string, recipients []consent.Recipient) ([]consent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	switch {
	case f.down:
		return nil, errors.New("core answered 503")
	case f.unknown:
		return nil, consent.ErrCategoryUnknown
	}
	results := make([]consent.Result, len(recipients))
	for i, r := range recipients {
		reason, ok := f.reasons[r.Email+r.Sub]
		switch {
		case !ok:
		case reason == "":
			results[i] = consent.Result{Matched: true, Allowed: true, UnsubscribeURL: "https://api.example.org/v1/communication-preferences/unsubscribe?t=x"}
		default:
			results[i] = consent.Result{Matched: true, Reason: reason}
		}
	}
	return results, nil
}

func (f *routeVerdicts) set(change func(*routeVerdicts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func preferenceWorld(t *testing.T, core *routeVerdicts) *approvalWorld {
	t.Helper()
	ring, err := unsubscribe.NewRing(1, map[byte][]byte{1: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	gate := mailer.PreferenceGate{On: true, Core: core, Links: unsubscribe.Config{On: true, BaseURL: "https://api.example.org/api/skymail", Ring: ring}}
	return newFormSourceWorld(t, purposesOn(), mailer.WithPreferenceGate(gate))
}

var duyuruFields = map[string]any{"purpose": "duyuru", "category": "club:duyuru", "commercial_content_absent": true}

func (w *approvalWorld) queueRow(task uuid.UUID, email string) (status, failure string, link *string) {
	w.t.Helper()
	if err := w.store.Conn.QueryRow(context.Background(), `
		SELECT status::text, coalesce(error, ''), list_unsubscribe_url FROM mail_queue WHERE task_id = $1 AND recipient_email = $2`,
		task, email).Scan(&status, &failure, &link); err != nil {
		w.t.Fatalf("row to %s: %v", email, err)
	}
	return status, failure, link
}

// core not answering refuses a list's send with 503 and Retry-After, and
// writes nothing; a Mail onayı request is refused at its approval and stays
// as it was, and is sent once core answers, with the verdict of the
// approval: a person who turned the category off after it was submitted is
// skipped, and the send's counts say so.
func TestTheBulkPreferenceCheckThroughTheRoutes(t *testing.T) {
	core := &routeVerdicts{reasons: map[string]string{"ayse@example.com": ""}, down: true}
	w := preferenceWorld(t, core)

	raw, err := json.Marshal(with(w.listSend(), duyuruFields))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", "yusuf")
	resp, err := w.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	var failure apiError
	_ = json.NewDecoder(resp.Body).Decode(&failure)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable || failure.Code != "mail_task.preference_unavailable" || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("core down: %d %s Retry-After %q, want 503 mail_task.preference_unavailable, 30", resp.StatusCode, failure.Code, resp.Header.Get("Retry-After"))
	}
	var sends int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks`).Scan(&sends); err != nil || sends != 0 {
		t.Fatalf("%d sends written (%v), want none", sends, err)
	}

	request := w.submit("elif", with(w.listSend(), duyuruFields))
	if status, _, failure := w.act("yusuf", request.ID, "approve", nil); status != fiber.StatusServiceUnavailable || failure.Code != "mail_task.preference_unavailable" {
		t.Fatalf("approve, core down: %d %s", status, failure.Code)
	}
	if _, after := w.get("yusuf", request.ID); after.State != "pending" {
		t.Fatalf("the request is %s after a refused approval, want pending", after.State)
	}

	// Mehmet turns the category off between the submission and the approval.
	core.set(func(f *routeVerdicts) { f.down, f.reasons["mehmet@example.com"] = false, consent.ReasonOptedOut })
	status, approved, failure := w.act("yusuf", request.ID, "approve", nil)
	if status != fiber.StatusOK || approved.State != "approved" {
		t.Fatalf("approve: %d %s %s", status, approved.State, failure.Code)
	}
	var task uuid.UUID
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT id FROM mail_tasks WHERE mail_list_id = $1`, w.list.ID).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if st, why, link := w.queueRow(task, "ayse@example.com"); st != "pending" || why != "" || link == nil {
		t.Errorf("Ayşe: %s %q %v, want pending with core's link", st, why, link)
	}
	if st, why, _ := w.queueRow(task, "mehmet@example.com"); st != "failed" || why != "preference: opted_out" {
		t.Errorf("Mehmet: %s %q, want failed preference: opted_out", st, why)
	}
	var send struct {
		Status          string `json:"status"`
		RecipientCounts struct {
			Failed   int `json:"failed"`
			OptedOut int `json:"opted_out"`
		} `json:"recipient_counts"`
	}
	if status, raw := w.call("baska", http.MethodGet, "/v1/mail_tasks/"+task.String(), nil, &send); status != fiber.StatusOK {
		t.Fatalf("the send: %d %s", status, raw)
	}
	if send.Status != "sending" || send.RecipientCounts.OptedOut != 1 || send.RecipientCounts.Failed != 1 {
		t.Errorf("the send reads %+v, want sending with one opted out", send)
	}
}

// A duyuru send that names its purpose states whether it is promotional; a
// team category core calls no team's is refused.
func TestTheBulkPreferenceCheckRefusals(t *testing.T) {
	core := &routeVerdicts{reasons: map[string]string{}}
	w := preferenceWorld(t, core)
	var failure apiError
	status, raw := w.call("yusuf", http.MethodPost, "/v1/mail_tasks", with(w.listSend(), map[string]any{"purpose": "duyuru", "category": "club:duyuru"}), &failure)
	if status != fiber.StatusBadRequest || failure.Code != "mail_task.commercial_statement_required" {
		t.Errorf("no statement: %d %s", status, raw)
	}
	core.set(func(f *routeVerdicts) { f.unknown = true })
	group := with(w.listSend(), map[string]any{"purpose": "duyuru", "category": "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru", "commercial_content_absent": true})
	group["mail_list_id"] = w.group
	failure = apiError{}
	status, raw = w.call("yusuf", http.MethodPost, "/v1/mail_tasks", group, &failure)
	if status != fiber.StatusBadRequest || failure.Code != "mail_task.category_unknown" {
		t.Errorf("no team: %d %s", status, raw)
	}
	var sends int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks`).Scan(&sends); err != nil || sends != 0 {
		t.Errorf("%d sends written (%v), want none", sends, err)
	}
}
