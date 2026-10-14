package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A queue row records the recipient's Keycloak subject when the send knew it
// (mail_queue.recipient_sub). Mail to Deniz at an address core no longer
// lists is still mail to Deniz: the subject finds it. The pending row goes,
// the sent one keeps its place in the send's counts with nothing about Deniz
// left on it, the subject included. Ayşe's row keeps hers.
func TestEraseAccountFindsMailToThePersonBySubject(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES
		    ('40000000-0000-4000-8000-00000000e101', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, recipient_sub) VALUES
		    ('50000000-0000-4000-8000-00000000e101', '40000000-0000-4000-8000-00000000e101', 'D. Y.', 'eski@example.org', 'Konu', 'Gövde', 'pending', '`+erasedSub+`'),
		    ('50000000-0000-4000-8000-00000000e102', '40000000-0000-4000-8000-00000000e101', 'D. Y.', 'eski@example.org', 'Konu', 'Gövde', 'sent', '`+erasedSub+`'),
		    ('50000000-0000-4000-8000-00000000e103', '40000000-0000-4000-8000-00000000e101', 'Ayşe Kaya', 'ayse@example.com', 'Konu', 'Gövde', 'sent', '`+otherSub+`');`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.EraseAccount(ctx, eraseDeniz(uuid.New())); err != nil {
		t.Fatal(err)
	}

	if n := queryCount(t, store, `SELECT count(*) FROM mail_queue WHERE id = '50000000-0000-4000-8000-00000000e101'`); n != 0 {
		t.Errorf("the pending row to Deniz's subject is still queued")
	}
	var name, email, subject, sub string
	if err := store.Conn.QueryRow(ctx, `
		SELECT recipient_full_name, recipient_email, subject, coalesce(recipient_sub::text, '')
		FROM mail_queue WHERE id = '50000000-0000-4000-8000-00000000e102'`).Scan(&name, &email, &subject, &sub); err != nil {
		t.Fatal(err)
	}
	if name != DeletedUserName || email != "" || subject != "" || sub != "" {
		t.Errorf("sent row to Deniz's subject = (%q, %q, %q, %q), want the stand-in's name and nothing else", name, email, subject, sub)
	}
	if got := queryString(t, store, `SELECT recipient_sub::text FROM mail_queue WHERE id = '50000000-0000-4000-8000-00000000e103'`); got == nil || *got != otherSub {
		t.Errorf("Ayşe's row's subject = %v, want hers kept", shown(got))
	}
}

// The names searched for in others' mail include the name a row found only by
// the subject carries: Deniz's old row, to an address core no longer lists,
// names them "Deniz Eskiad", and Ayşe's mail that names Deniz so is cleared.
func TestEraseAccountSearchesForTheNameOnARowFoundBySubject(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES
		    ('40000000-0000-4000-8000-00000000e201', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, recipient_sub) VALUES
		    ('50000000-0000-4000-8000-00000000e201', '40000000-0000-4000-8000-00000000e201', 'Deniz Eskiad', 'eski@example.org', 'Konu', 'Gövde', 'sent', '`+erasedSub+`'),
		    ('50000000-0000-4000-8000-00000000e202', '40000000-0000-4000-8000-00000000e201', 'Ayşe Kaya', 'ayse@example.com', 'Not', 'Deniz Eskiad ile görüşme', 'sent', NULL);`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.EraseAccount(ctx, eraseDeniz(uuid.New())); err != nil {
		t.Fatal(err)
	}

	var subject, body string
	if err := store.Conn.QueryRow(ctx, `
		SELECT subject, body FROM mail_queue WHERE id = '50000000-0000-4000-8000-00000000e202'`).Scan(&subject, &body); err != nil {
		t.Fatal(err)
	}
	if subject != "" || body != "" {
		t.Errorf("Ayşe's mail naming Deniz by the name on their subject's row = (%q, %q), want it cleared", subject, body)
	}
}
