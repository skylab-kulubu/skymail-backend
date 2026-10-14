package main

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// A send to a Keycloak group knows who each member is: every row it queues
// records the member's Keycloak subject (mail_queue.recipient_sub) and goes
// out as an e-mail. A send to people named by address knows only the
// address.
func TestAGroupSendRecordsEachMembersSubject(t *testing.T) {
	w := newApprovalWorld(t)

	send := w.listSend()
	send["mail_list_id"] = w.group
	toGroup := w.submit("elif", send)
	if status, _, failure := w.act("fatih", toGroup.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve group send = %d %+v", status, failure)
	}
	delete(send, "mail_list_id")
	send["recipients"] = []approvalRecipient{{Email: "konusmaci@example.com", FullName: "Konuşmacı"}}
	toOne := w.submit("elif", send)
	if status, _, failure := w.act("fatih", toOne.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve single send = %d %+v", status, failure)
	}

	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 2 {
		t.Fatalf("sends = %+v, want the group's and the person's", sent)
	}
	want := map[string]string{
		baska.email:             baska.sub,
		elif.email:              elif.sub,
		"konusmaci@example.com": "",
	}
	rows, err := w.store.Conn.Query(context.Background(), `
		SELECT recipient_email, channel, coalesce(recipient_sub::text, '')
		FROM mail_queue WHERE task_id = ANY ($1)`, []uuid.UUID{sent[0].taskID, sent[1].taskID})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var email, channel, sub string
		if err := rows.Scan(&email, &channel, &sub); err != nil {
			t.Fatal(err)
		}
		seen++
		if wantSub, ok := want[email]; !ok || sub != wantSub || channel != "email" {
			t.Errorf("row to %s: channel %q, subject %q; want email and %q", email, channel, sub, wantSub)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Errorf("%d rows queued, want %d", seen, len(want))
	}
}
