package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// The counts of a send to fewer than 10 tracked people, and of mail the
// system sends, tell of people: GET /:id/tracking gives them only to one who
// may read the send's people (hub review T6; KVKK notice s3, §4). Anyone else
// with mails:read gets withheld and no report.
func TestTrackingCountsOfASmallSendGoOnlyToWhoMayReadItsPeople(t *testing.T) {
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
	approvalPeople["takip_okur"] = approvalPerson{sub: uuid.NewString(), roles: []string{"skymail:access", "skymail:mails:read", handlers.TrackingReadRole}}
	approvalPeople["yusuf_okur"] = approvalPerson{sub: yusuf.sub, name: yusuf.name, email: yusuf.email, roles: append(append([]string{}, yusuf.roles...), "skymail:mails:read")}
	defer delete(approvalPeople, "takip_okur")
	defer delete(approvalPeople, "yusuf_okur")
	base := "/v1/mail_tasks/" + created.ID.String() + "/tracking"

	read := func(who string) (withheld bool, report json.RawMessage) {
		t.Helper()
		var summary struct {
			Withheld bool            `json:"withheld"`
			Report   json.RawMessage `json:"report"`
		}
		if status, raw := w.call(who, "GET", base, nil, &summary); status != 200 {
			t.Fatalf("summary as %s: %d %s", who, status, raw)
		}
		return summary.Withheld, summary.Report
	}
	expect := func(stage, who string, wantWithheld bool) {
		t.Helper()
		withheld, report := read(who)
		if withheld != wantWithheld || (string(report) == "null") != wantWithheld {
			t.Errorf("%s, as %s: withheld %v, report %s; want withheld %v", stage, who, withheld, report, wantWithheld)
		}
	}

	// A person's small send: its sender and the tracking role read the counts, another mails:read does not.
	expect("small send", "baska", true)
	expect("small send", "yusuf_okur", false)
	expect("small send", "takip_okur", false)

	// Mail the system sends: only the role, not the person it names as sender.
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET priority=1 WHERE task_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	expect("system mail", "baska", true)
	expect("system mail", "yusuf_okur", true)
	expect("system mail", "takip_okur", false)

	// Retained system mail totals (>= 10) still go to the role only.
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET sent_at=now()-interval '366 days' WHERE task_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Conn.Exec(ctx, `INSERT INTO mail_task_tracking_totals(task_id,tracked_recipients,unique_opens,unique_likely_human_opens,unique_clickers) VALUES($1,10,8,5,3)`, created.ID); err != nil {
		t.Fatal(err)
	}
	expect("retained system mail", "baska", true)
	expect("retained system mail", "takip_okur", false)

	// Out of the system lane, a retained >= 10 total is anyone's with mails:read.
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET priority=0 WHERE task_id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	expect("retained person's send", "baska", false)
}
