package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

func TestTrackingReadRoutesEnforceRolesSenderAndNinetyDays(t *testing.T) {
	w, _ := trackedWorld(t, true)
	ctx := context.Background()
	send := w.listSend()
	send["mail_list_id"] = w.group
	send["tracking"] = true
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if status, raw := w.call("yusuf", "POST", "/v1/mail_tasks", send, &created); status != 201 {
		t.Fatalf("create: %d %s", status, raw)
	}
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET status='sent',sent_at=now()-interval '2 minutes' WHERE task_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/mail_tasks/" + created.ID.String() + "/tracking"
	for who, want := range map[string]int{"yusuf": 403, "baska": 200, "fatih": 403} {
		var result map[string]any
		status, raw := w.call(who, "GET", base, nil, &result)
		if status != want {
			t.Errorf("summary as %s: %d %s", who, status, raw)
		}
		if status == 200 && (result["tracking_available"] != true || result["tracking_requested"] != true || result["retained"] != false) {
			t.Errorf("summary contract: %s", raw)
		}
	}
	approvalPeople["tracking_admin"] = approvalPerson{sub: uuid.NewString(), roles: []string{"skymail:access", handlers.TrackingReadRole}}
	defer delete(approvalPeople, "tracking_admin")
	for who, want := range map[string]int{"yusuf": 200, "baska": 403, "fatih": 403, "tracking_admin": 200} {
		status, raw := w.call(who, "GET", base+"/recipients", nil, nil)
		if status != want {
			t.Errorf("recipients as %s: %d %s", who, status, raw)
		}
		if status == 200 && (bytes.Contains(raw, []byte("tracking_token")) || bytes.Contains(raw, []byte("link_token"))) {
			t.Errorf("token exposed: %s", raw)
		}
	}
	for _, query := range []string{"?filter=invalid", "?page=0", "?page_size=101"} {
		if status, _ := w.call("yusuf", "GET", base+"/recipients"+query, nil, nil); status != 400 {
			t.Errorf("%s status %d", query, status)
		}
	}
	// A year (tracking.EventRetention; Yusuf, 2026-10-19), not 90 days.
	for _, age := range []string{"89 days", "91 days", "364 days", "365 days", "366 days"} {
		if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET sent_at=now()-$2::interval WHERE task_id=$1`, created.ID, age); err != nil {
			t.Fatal(err)
		}
		want := 410
		if age == "89 days" || age == "91 days" || age == "364 days" {
			want = 200
		}
		if status, raw := w.call("yusuf", "GET", base+"/recipients", nil, nil); status != want {
			t.Errorf("%s: %d %s", age, status, raw)
		}
	}
	var summary struct {
		Retained bool            `json:"retained"`
		Report   json.RawMessage `json:"report"`
	}
	status, raw := w.call("baska", "GET", base, nil, &summary)
	if status != 200 || !summary.Retained || string(summary.Report) != "null" {
		t.Fatalf("expired without retained totals: %d %s", status, raw)
	}
	// A retained >=10-person total remains readable without identity records.
	if _, err := w.store.Conn.Exec(ctx, `INSERT INTO mail_task_tracking_totals(task_id,tracked_recipients,unique_opens,unique_likely_human_opens,unique_clickers) VALUES($1,10,8,5,3)`, created.ID); err != nil {
		t.Fatal(err)
	}
	status, raw = w.call("baska", "GET", base, nil, &summary)
	var retained map[string]any
	_ = json.Unmarshal(summary.Report, &retained)
	if status != 200 || retained["tracked_recipients"] != float64(10) {
		t.Fatalf("retained counts: %d %s", status, raw)
	}
	if status, _ := w.call("baska", "GET", base+"/recipients", nil, nil); status != 403 {
		t.Error("expired existence revealed before recipient authorization")
	}
}

func TestPublicTrackingRoutesUseDatabaseWithoutGeneralErrorLogging(t *testing.T) {
	w, _ := trackedWorld(t, true)
	send := w.listSend()
	send["mail_list_id"] = w.group
	send["tracking"] = true
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if status, raw := w.call("yusuf", "POST", "/v1/mail_tasks", send, &created); status != 201 {
		t.Fatalf("%d %s", status, raw)
	}
	ctx := context.Background()
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET status='sent',sent_at=now()-interval '2 minutes' WHERE task_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	_, rows := w.trackingOf(created.ID)
	rt := tracking.EncodeToken(rows[elif.email].token)
	ltBytes, _ := tracking.NewLinkToken()
	lt := tracking.EncodeToken(ltBytes)
	if _, err := w.store.Conn.Exec(ctx, `INSERT INTO mail_task_links(task_id,link_token,url,position) VALUES($1,$2,'https://destination.invalid/registered',99)`, created.ID, ltBytes); err != nil {
		t.Fatal(err)
	}
	handlers.NewTrackingPublicHandler(w.store, tracking.Config{Mode: tracking.ModeOn}).Register(w.app)
	for _, method := range []string{"HEAD", "GET"} {
		if status, _ := w.call("yusuf", method, "/t/c/"+rt+"/"+lt, nil, nil); status != fiber.StatusFound {
			t.Fatalf("%s click=%d", method, status)
		}
		var count int
		if err := w.store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_tracking_events WHERE task_id=$1 AND kind='click'`, created.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if method == "GET" {
			want = 1
		}
		if count != want {
			t.Errorf("%s click writes=%d", method, count)
		}
	}
	if status, _ := w.call("yusuf", "GET", "/t/o/"+rt, nil, nil); status != 200 {
		t.Fatal("pixel failed")
	}
	// Old tracking links remain usable when the process flag is off.
	off := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	handlers.NewTrackingPublicHandler(w.store, tracking.Config{Mode: tracking.ModeOff}).Register(off)
	saved := w.app
	w.app = off
	if status, _ := w.call("yusuf", "GET", "/t/c/"+rt+"/"+lt, nil, nil); status != 302 {
		t.Fatalf("off click=%d", status)
	}
	w.app = saved
	// The public unit tests exercise all errors and panic logging; here the
	// real resolver proves persisted cleared targets cannot redirect.
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_task_links SET url=NULL,url_cleared_reason='erased' WHERE link_token=$1`, ltBytes); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.call("yusuf", "GET", "/t/c/"+rt+"/"+lt, nil, nil); status != 410 {
		t.Fatalf("erased click=%d", status)
	}
}
