package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sweepSend is a send for the content sweep: its template's content_sensitive,
// whether it went to a list, how long ago it was created and its rows'
// statuses. Every row holds a rendered mail.
type sweepSend struct {
	sensitive *bool
	list      bool
	age       time.Duration
	statuses  []string
}

func seedSweepSend(t *testing.T, db *Store, send sweepSend) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var templateID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('Şablon', 'Konu', '<p>x</p>', 'x', '', $1) RETURNING id`, send.sensitive).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	var list *uuid.UUID
	if send.list {
		id := uuid.New()
		list = &id
	}
	var taskID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, created_at)
		VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', $1, $2, '{"code":"123456"}', now() - $3::interval)
		RETURNING id`, templateID, list, send.age.String()).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	for _, status := range send.statuses {
		if _, err := db.Conn.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
			VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Kodun 123456', 'Kod: 123456', '<p>123456</p>', $2, now() - $3::interval)`,
			taskID, status, send.age.String()); err != nil {
			t.Fatal(err)
		}
	}
	return taskID
}

type sweptState struct {
	cleared   bool
	variables string
	content   int
}

func sweptStateOf(t *testing.T, db *Store, task uuid.UUID) sweptState {
	t.Helper()
	var state sweptState
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT mt.content_cleared_at IS NOT NULL,
		       mt.body_variables::text,
		       (SELECT count(*) FROM mail_queue q
		        WHERE q.task_id = mt.id AND (q.subject <> '' OR q.body <> '' OR q.body_html IS NOT NULL))
		FROM mail_tasks mt WHERE mt.id = $1`, task).Scan(&state.cleared, &state.variables, &state.content); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestContentSweepEmptiesSensitiveSendsAtOnceAndOthersAfterRetention(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	yes, no := true, false
	hour, day := time.Hour, 24*time.Hour

	// Sensitive, done: emptied at once (past the five minutes the rows have to be written).
	singleDone := seedSweepSend(t, db, sweepSend{age: hour, statuses: []string{"sent"}})
	markedDone := seedSweepSend(t, db, sweepSend{sensitive: &yes, list: true, age: hour, statuses: []string{"sent", "failed"}})
	// Sensitive, but a row is still to go out, or the send is a minute old: kept for now.
	singleRetrying := seedSweepSend(t, db, sweepSend{age: hour, statuses: []string{"pending"}})
	singleProcessing := seedSweepSend(t, db, sweepSend{age: hour, statuses: []string{"sent", "processing"}})
	singleJustNow := seedSweepSend(t, db, sweepSend{age: time.Minute, statuses: []string{"sent"}})
	// Shown: kept for the retention period, emptied past it.
	listRecent := seedSweepSend(t, db, sweepSend{list: true, age: 89 * day, statuses: []string{"sent"}})
	listOld := seedSweepSend(t, db, sweepSend{list: true, age: 91 * day, statuses: []string{"sent", "failed"}})
	shownSingleRecent := seedSweepSend(t, db, sweepSend{sensitive: &no, age: hour, statuses: []string{"sent"}})
	shownSingleOld := seedSweepSend(t, db, sweepSend{sensitive: &no, age: 91 * day, statuses: []string{"sent"}})

	sends, rows, err := db.ClearMailContent(ctx, 90, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 4 || rows != 6 {
		t.Fatalf("swept %d sends, %d rows; want 4 sends, 6 rows", sends, rows)
	}
	for name, task := range map[string]uuid.UUID{
		"single done": singleDone, "marked sensitive": markedDone, "list past retention": listOld, "shown single past retention": shownSingleOld,
	} {
		if state := sweptStateOf(t, db, task); !state.cleared || state.variables != "{}" || state.content != 0 {
			t.Errorf("%s: %+v, want emptied with its variables", name, state)
		}
	}
	for name, task := range map[string]uuid.UUID{
		"single retrying": singleRetrying, "single processing": singleProcessing, "single just now": singleJustNow,
		"list within retention": listRecent, "shown single within retention": shownSingleRecent,
	} {
		if state := sweptStateOf(t, db, task); state.cleared || state.variables == "{}" || state.content == 0 {
			t.Errorf("%s: %+v, want kept", name, state)
		}
	}

	// Done once: a second sweep finds nothing.
	sends, rows, err = db.ClearMailContent(ctx, 90, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || rows != 0 {
		t.Fatalf("second sweep emptied %d sends, %d rows; want none", sends, rows)
	}

	// The status, the recipient and the error stay: the send's counts and who it went to.
	var sent, failed int
	var email string
	if err := db.Conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'sent'), count(*) FILTER (WHERE status = 'failed'), min(recipient_email)
		FROM mail_queue WHERE task_id = $1`, listOld).Scan(&sent, &failed, &email); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || failed != 1 || email != "ayse@example.com" {
		t.Errorf("an emptied send's rows are %d sent, %d failed, to %q; want 1, 1, ayse@example.com", sent, failed, email)
	}
}

// A send to a person an approved Mail onayı request queued is bulk: what the
// club wrote, shown as a list send is.
func TestApprovedSendToAPersonIsBulk(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	task := seedSweepSend(t, db, sweepSend{age: time.Hour, statuses: []string{"sent"}})
	var bulk, sensitive bool
	if err := db.Conn.QueryRow(ctx, `SELECT mail_task_bulk($1), mail_task_content_sensitive($1)`, task).Scan(&bulk, &sensitive); err != nil {
		t.Fatal(err)
	}
	if bulk || !sensitive {
		t.Fatalf("a single mail: bulk %v, sensitive %v; want false, true", bulk, sensitive)
	}
	// The link alone decides; the request's own rows (its version, its
	// people) are left out, so its foreign keys and deferred checks are off.
	tx, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		WITH approval AS (
			INSERT INTO mail_approvals (submitter_sub, template_id, template_version_id, body_variables,
			                            created_at, submitted_at, deadline_at, updated_at, state)
			SELECT 'sub', mt.template_id, gen_random_uuid(), '{}', now(), now(), now() + INTERVAL '7 days', now(), 'approved'
			FROM mail_tasks mt WHERE mt.id = $1
			RETURNING id)
		INSERT INTO mail_approval_tasks (approval_id, position, task_id)
		SELECT id, 1, $1 FROM approval`, task); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Conn.QueryRow(ctx, `SELECT mail_task_bulk($1), mail_task_content_sensitive($1)`, task).Scan(&bulk, &sensitive); err != nil {
		t.Fatal(err)
	}
	if !bulk || sensitive {
		t.Fatalf("an approved send: bulk %v, sensitive %v; want true, false", bulk, sensitive)
	}
	// A task that is not there is sensitive.
	if err := db.Conn.QueryRow(ctx, `SELECT mail_task_content_sensitive(gen_random_uuid())`).Scan(&sensitive); err != nil {
		t.Fatal(err)
	}
	if !sensitive {
		t.Fatal("a missing task is shown; want sensitive")
	}
}
