package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A send's events (mail_task_events, skymail-yeni-tasarim B3).

// Every retry is counted in its send's retried event of the hour: one row a
// send and hour, however many retries, the first and the last retry's time
// on it. Another send's retries are its own.
func TestRetriesAreCountedInOneEventAnHour(t *testing.T) {
	store := lifecycleStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('10000000-0000-4000-8000-000000000001', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', '');
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES
		    ('40000000-0000-4000-8000-0000000000e1', 'sender', '10000000-0000-4000-8000-000000000001', '{}'),
		    ('40000000-0000-4000-8000-0000000000e2', 'sender', '10000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status) VALUES
		    ('50000000-0000-4000-8000-0000000000e1', '40000000-0000-4000-8000-0000000000e1', 'A', 'a@example.com', 'D', 'x', 'pending'),
		    ('50000000-0000-4000-8000-0000000000e2', '40000000-0000-4000-8000-0000000000e1', 'B', 'b@example.com', 'D', 'x', 'pending'),
		    ('50000000-0000-4000-8000-0000000000e3', '40000000-0000-4000-8000-0000000000e2', 'C', 'c@example.com', 'D', 'x', 'pending');`); err != nil {
		t.Fatal(err)
	}
	retry := func(row string) {
		t.Helper()
		claimedAt := time.Now().UTC().Truncate(time.Microsecond)
		if _, err := store.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'processing', claimed_by = 'replica', claimed_at = $2 WHERE id = $1`,
			row, claimedAt); err != nil {
			t.Fatal(err)
		}
		reason := "421 4.7.0"
		if _, err := store.RescheduleMailQueueItem(ctx, RescheduleMailQueueItemParams{
			ID: uuid.MustParse(row), ClaimedBy: "replica", ClaimedAt: claimedAt, Error: &reason, DelaySeconds: 60,
		}); err != nil {
			t.Fatal(err)
		}
	}
	retry("50000000-0000-4000-8000-0000000000e1")
	retry("50000000-0000-4000-8000-0000000000e2")
	retry("50000000-0000-4000-8000-0000000000e1")
	retry("50000000-0000-4000-8000-0000000000e3")

	events, err := store.ListMailTaskEvents(ctx, uuid.MustParse("40000000-0000-4000-8000-0000000000e1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "retried" || events[0].Count == nil || *events[0].Count != 3 ||
		events[0].ActorSub != nil || events[0].LastAt.Before(events[0].CreatedAt) {
		t.Fatalf("the send's events = %+v, want one retried event counting 3, by SkyMail", events)
	}
	other, err := store.ListMailTaskEvents(ctx, uuid.MustParse("40000000-0000-4000-8000-0000000000e2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Count == nil || *other[0].Count != 1 {
		t.Fatalf("the other send's events = %+v, want its one retry", other)
	}
	if n := queryCount(t, store, `SELECT attempts FROM mail_queue WHERE id = '50000000-0000-4000-8000-0000000000e1'`); n != 2 {
		t.Errorf("the row retried twice counts %d attempts", n)
	}

	// A claim that lost its lease retries nothing and counts nothing.
	reason := "421"
	if _, err := store.RescheduleMailQueueItem(ctx, RescheduleMailQueueItemParams{
		ID: uuid.MustParse("50000000-0000-4000-8000-0000000000e2"), ClaimedBy: "replica", ClaimedAt: time.Now(), Error: &reason, DelaySeconds: 60,
	}); err == nil {
		t.Fatal("a reschedule of a row not claimed succeeded")
	}
	if n := queryCount(t, store, `SELECT count FROM mail_task_events WHERE task_id = '40000000-0000-4000-8000-0000000000e1'`); n != 3 {
		t.Errorf("a lost lease counted a retry: %d", n)
	}
}

// Account erasure replaces who paused, resumed or cancelled a send, in its
// events and on the send itself (paused_by, cancelled_by), with Silinmiş
// kullanıcı. Another person's stays, and SkyMail's own events are left be.
func TestEraseAccountReplacesWhoControlledASend(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, paused_at, paused_reason, paused_by, cancelled_at, cancelled_by) VALUES
		    ('40000000-0000-4000-8000-0000000000f1', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}', now(), 'operator', '`+erasedSub+`', NULL, NULL),
		    ('40000000-0000-4000-8000-0000000000f2', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}', NULL, NULL, NULL, now(), '`+erasedSub+`'),
		    ('40000000-0000-4000-8000-0000000000f3', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}', now(), 'operator', '`+otherSub+`', NULL, NULL);
		INSERT INTO mail_task_events (id, task_id, kind, reason, actor_sub, actor_name, count, hour) OVERRIDING SYSTEM VALUE VALUES
		    (1, '40000000-0000-4000-8000-0000000000f1', 'paused', 'operator', '`+erasedSub+`', 'Deniz Yılmaz', NULL, NULL),
		    (2, '40000000-0000-4000-8000-0000000000f2', 'cancelled', NULL, '`+erasedSub+`', 'Deniz Yılmaz', 2, NULL),
		    (3, '40000000-0000-4000-8000-0000000000f3', 'paused', 'operator', '`+otherSub+`', 'Ayşe Kaya', NULL, NULL),
		    (4, '40000000-0000-4000-8000-0000000000f1', 'retried', NULL, NULL, NULL, 5, date_trunc('hour', now()));`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.EraseAccount(ctx, eraseDeniz(uuid.New())); err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct {
		sql  string
		want *string
	}{
		{`SELECT paused_by FROM mail_tasks WHERE id = '40000000-0000-4000-8000-0000000000f1'`, ptrTo(DeletedUserSubject)},
		{`SELECT cancelled_by FROM mail_tasks WHERE id = '40000000-0000-4000-8000-0000000000f2'`, ptrTo(DeletedUserSubject)},
		{`SELECT paused_by FROM mail_tasks WHERE id = '40000000-0000-4000-8000-0000000000f3'`, ptrTo(otherSub)},
		{`SELECT actor_sub FROM mail_task_events WHERE id = 1`, ptrTo(DeletedUserSubject)},
		{`SELECT actor_name FROM mail_task_events WHERE id = 1`, ptrTo(DeletedUserName)},
		{`SELECT actor_sub FROM mail_task_events WHERE id = 2`, ptrTo(DeletedUserSubject)},
		{`SELECT actor_name FROM mail_task_events WHERE id = 2`, ptrTo(DeletedUserName)},
		{`SELECT actor_sub FROM mail_task_events WHERE id = 3`, ptrTo(otherSub)},
		{`SELECT actor_name FROM mail_task_events WHERE id = 3`, ptrTo("Ayşe Kaya")},
		{`SELECT actor_sub FROM mail_task_events WHERE id = 4`, nil},
	} {
		got := queryString(t, store, check.sql)
		if (got == nil) != (check.want == nil) || (got != nil && *got != *check.want) {
			t.Errorf("%s = %v, want %v", check.sql, shown(got), shown(check.want))
		}
	}
}
