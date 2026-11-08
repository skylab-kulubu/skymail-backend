package main

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// An approved request tells its submitter how its sends went, in totals only,
// without skymail:mails:read (hub ticket ekip-postasi 09): sent, skipped,
// failed and pending over every send, one total for a request to people, and
// no person in it. Opens and clicks follow GET /mail_tasks/{id}/tracking's
// rule (ADR-0074 decision 9, addendum 2026-11-08): fewer than 10 tracked are
// withheld from the submitter and an approver alike, not from the tracking
// role. A request that went nowhere has no result; who may read a request is
// unchanged.
func TestAnApprovedRequestTellsHowItWentInTotalsOnly(t *testing.T) {
	w := newApprovalWorld(t)
	ctx := context.Background()
	approvalPeople["takip_onayci"] = approvalPerson{sub: uuid.NewString(), name: "Takip Onaycısı", email: "takip@yildizskylab.com",
		roles: []string{"skymail:access", "skymail:mails:approve", handlers.TrackingReadRole}}
	defer delete(approvalPeople, "takip_onayci")

	type result struct {
		Sent     int64 `json:"sent"`
		Skipped  int64 `json:"skipped"`
		Failed   int64 `json:"failed"`
		Pending  int64 `json:"pending"`
		Tracking *struct {
			TrackedRecipients int `json:"tracked_recipients"`
			Opened            int `json:"opened"`
			Clicked           int `json:"clicked"`
		} `json:"tracking"`
		TrackingWithheld bool `json:"tracking_withheld"`
	}
	read := func(who string, id uuid.UUID) (json.RawMessage, *result) {
		t.Helper()
		var answer struct {
			Result json.RawMessage `json:"result"`
		}
		status, raw := w.call(who, fiber.MethodGet, "/v1/mail_approvals/"+id.String(), nil, &answer)
		mustStatus(t, "read as "+who, status, fiber.StatusOK, raw)
		if answer.Result == nil {
			return nil, nil
		}
		var r result
		if err := json.Unmarshal(answer.Result, &r); err != nil {
			t.Fatal(err)
		}
		return answer.Result, &r
	}

	send := w.listSend()
	delete(send, "mail_list_id")
	people := []approvalRecipient{
		{Email: "ayse@yildizskylab.com", FullName: "Ayşe Kaya"},
		{Email: "mehmet@yildizskylab.com", FullName: "Mehmet Demir"},
		{Email: "zeynep@yildizskylab.com", FullName: "Zeynep Ak"},
		{Email: "can@yildizskylab.com", FullName: "Can Er"},
	}
	send["recipients"] = people
	request := w.submit("elif", send)
	if raw, _ := read("elif", request.ID); raw != nil {
		t.Errorf("pending request has result %s", raw)
	}

	if status, _, failure := w.act("fatih", request.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	_, approved := w.get("elif", request.ID)
	if len(approved.TaskIDs) != len(people) {
		t.Fatalf("task_ids = %v, want one send per person", approved.TaskIDs)
	}
	for i, outcome := range []struct{ status, err string }{
		{"sent", ""}, {"failed", "suppressed: unsubscribed"}, {"failed", "smtp_rejected: 550 5.1.1"}, {"pending", ""},
	} {
		if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET status = $2::mail_queue_status, error = NULLIF($3::text, ''), sent_at = CASE WHEN $4::boolean THEN now() END WHERE task_id = $1`,
			approved.TaskIDs[i], outcome.status, outcome.err, outcome.status == "sent"); err != nil {
			t.Fatal(err)
		}
	}

	raw, got := read("elif", request.ID)
	if got == nil || got.Sent != 1 || got.Skipped != 1 || got.Failed != 1 || got.Pending != 1 || got.Tracking != nil || got.TrackingWithheld {
		t.Fatalf("submitter's result = %s, want 1 sent, 1 skipped, 1 failed, 1 pending, no tracking", raw)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"failed", "pending", "sent", "skipped", "tracking", "tracking_withheld"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("result fields = %v, want %v: totals only, no row per send", keys, want)
	}
	for _, p := range people {
		if strings.Contains(string(raw), p.Email) || strings.Contains(string(raw), p.FullName) {
			t.Errorf("result %s names %s", raw, p.Email)
		}
	}
	if status, _ := w.get("baska", request.ID); status != fiber.StatusNotFound {
		t.Errorf("another member reads the request: %d, want 404", status)
	}

	// Tracked, four people: their counts tell of them. The submitter and an
	// approver without the tracking role get them withheld; the role reads them.
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_tasks SET tracking_requested = true WHERE id = ANY($1)`, approved.TaskIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET tracking_token = decode(md5(id::text), 'hex'), sent_at = now() WHERE task_id = ANY($1)`, approved.TaskIDs); err != nil {
		t.Fatal(err)
	}
	for who, withheld := range map[string]bool{"elif": true, "fatih": true, "takip_onayci": false} {
		raw, got := read(who, request.ID)
		if got == nil || got.TrackingWithheld != withheld || (got.Tracking == nil) != withheld ||
			(!withheld && got.Tracking.TrackedRecipients != len(people)) {
			t.Errorf("tracked result as %s = %s, want withheld %v", who, raw, withheld)
		}
	}

	// A rejected request went nowhere: no result.
	rejected := w.submit("elif", send)
	if status, _, failure := w.act("fatih", rejected.ID, "reject", map[string]any{"reason": "Tarih yanlış."}); status != fiber.StatusOK {
		t.Fatalf("reject = %d %+v", status, failure)
	}
	if raw, _ := read("elif", rejected.ID); raw != nil {
		t.Errorf("rejected request has result %s", raw)
	}
}
