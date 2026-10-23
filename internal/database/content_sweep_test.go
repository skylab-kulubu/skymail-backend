package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sweepSend is a send for the content sweep: its template's content_sensitive,
// whether it went to a list, how long ago it was created and its rows'
// statuses. Every row holds a rendered mail and an unsubscribe link, which go
// together.
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
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, created_at, content_sensitive_at_send)
		VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', $1, $2, '{"code":"123456"}', now() - $3::interval,
		        COALESCE($4::boolean::text, 'unset'))
		RETURNING id`, templateID, list, send.age.String(), send.sensitive).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	for _, status := range send.statuses {
		if _, err := db.Conn.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at,
			                        list_unsubscribe_url)
			VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Kodun 123456', 'Kod: 123456', '<p>123456</p>', $2, now() - $3::interval,
			        'https://api.example.com/v1/communication-preferences/unsubscribe?t=AQ')`,
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
		        WHERE q.task_id = mt.id AND (q.subject <> '' OR q.body <> '' OR q.body_html IS NOT NULL
		                                     OR q.list_unsubscribe_url IS NOT NULL))
		FROM mail_tasks mt WHERE mt.id = $1`, task).Scan(&state.cleared, &state.variables, &state.content); err != nil {
		t.Fatal(err)
	}
	return state
}

// Every send is kept for the retention period, sensitive or not: a sensitive
// send's content is shown masked (2026-10-13), no longer emptied at once.
func TestContentSweepKeepsEverySendForTheRetentionPeriod(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	yes, no := true, false
	hour, day := time.Hour, 24*time.Hour

	// Sensitive, done, within retention: kept, as any other send.
	singleDone := seedSweepSend(t, db, sweepSend{age: hour, statuses: []string{"sent"}})
	markedDone := seedSweepSend(t, db, sweepSend{sensitive: &yes, list: true, age: hour, statuses: []string{"sent", "failed"}})
	singleRetrying := seedSweepSend(t, db, sweepSend{age: hour, statuses: []string{"pending"}})
	singleJustNow := seedSweepSend(t, db, sweepSend{age: time.Minute, statuses: []string{"sent"}})
	listRecent := seedSweepSend(t, db, sweepSend{list: true, age: 89 * day, statuses: []string{"sent"}})
	shownSingleRecent := seedSweepSend(t, db, sweepSend{sensitive: &no, age: hour, statuses: []string{"sent"}})
	// Past retention: emptied, sensitive or not, once nothing is left to go out.
	singleOld := seedSweepSend(t, db, sweepSend{age: 91 * day, statuses: []string{"sent"}})
	listOld := seedSweepSend(t, db, sweepSend{list: true, age: 91 * day, statuses: []string{"sent", "failed"}})
	shownSingleOld := seedSweepSend(t, db, sweepSend{sensitive: &no, age: 91 * day, statuses: []string{"sent"}})
	oldProcessing := seedSweepSend(t, db, sweepSend{age: 91 * day, statuses: []string{"sent", "processing"}})

	sends, rows, err := db.ClearMailContent(ctx, 90, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 3 || rows != 4 {
		t.Fatalf("swept %d sends, %d rows; want 3 sends, 4 rows", sends, rows)
	}
	for name, task := range map[string]uuid.UUID{
		"sensitive single past retention": singleOld, "list past retention": listOld, "shown single past retention": shownSingleOld,
	} {
		if state := sweptStateOf(t, db, task); !state.cleared || state.variables != "{}" || state.content != 0 {
			t.Errorf("%s: %+v, want emptied with its variables", name, state)
		}
	}
	for name, task := range map[string]uuid.UUID{
		"sensitive single done": singleDone, "marked sensitive": markedDone, "single retrying": singleRetrying,
		"single just now": singleJustNow, "list within retention": listRecent, "shown single within retention": shownSingleRecent,
		"past retention, a row still going out": oldProcessing,
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

// Account erasure empties a recipient's row of a sensitive send while the
// sweep is masking that send (#63 re-review R1): the sweep's reading waits for
// the erasure and then does not take the emptied row, and its writing never
// puts a mail back into an emptied row.
func TestMaskingNeverWritesBackARowErasureEmptied(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	yes := true
	task := seedSweepSend(t, db, sweepSend{sensitive: &yes, list: true, age: time.Hour, statuses: []string{"sent", "sent"}})
	var ids []uuid.UUID
	rows, err := db.Conn.Query(ctx, `SELECT id FROM mail_queue WHERE task_id = $1 ORDER BY id`, task)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	// Erasure empties the rows, not committed yet.
	erasure, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = erasure.Rollback(ctx) }()
	if _, err := erasure.Exec(ctx, `UPDATE mail_queue SET recipient_full_name = 'Silinmiş kullanıcı', recipient_email = '',
		subject = '', body = '', body_html = NULL WHERE task_id = $1`, task); err != nil {
		t.Fatal(err)
	}

	sweep, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sweep.Rollback(ctx) }()
	q := New(sweep)
	sends, err := q.TakeSendToMask(ctx, []uuid.UUID{})
	if err != nil || len(sends) != 1 || sends[0].ID != task || !sends[0].Sensitive {
		t.Fatalf("sends %+v err %v", sends, err)
	}
	read := make(chan []ListQueueRowsToMaskRow, 1)
	failed := make(chan error, 1)
	go func() {
		rows, err := q.ListQueueRowsToMask(ctx, ListQueueRowsToMaskParams{TaskID: task, RowLimit: 10})
		if err != nil {
			failed <- err
			return
		}
		read <- rows
	}()
	select {
	case rows := <-read:
		t.Fatalf("the sweep read %d rows while erasure held them", len(rows))
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := erasure.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case rows := <-read:
		if len(rows) != 0 {
			t.Fatalf("the sweep took %d rows erasure emptied", len(rows))
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep's reading never came back")
	}

	// A write from a reading made before the erasure does not land.
	if err := q.SetMaskedQueueContent(ctx, SetMaskedQueueContentParams{
		TaskID:   task,
		Ids:      ids,
		Subjects: []string{"Kodun ••••••", "Kodun ••••••"},
		Bodies:   []string{"Merhaba Ayşe Kaya", "Merhaba Ayşe Kaya"},
		Htmls:    []string{"<p>Merhaba Ayşe Kaya</p>", "<p>Merhaba Ayşe Kaya</p>"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sweep.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND (subject <> '' OR body <> '' OR body_html IS NOT NULL)`, task).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("%d erased rows hold a mail again", held)
	}
}

// The sweep marks a send not sensitive and, before it commits, the send's
// template is made sensitive (#63 Ek 2, Y1): the template's write waits for
// the sweep and makes the send due again, so it is masked.
func TestATemplateMadeSensitiveWhileTheSweepDecidesMakesTheSendDue(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	no := false
	task := seedSweepSend(t, db, sweepSend{sensitive: &no, list: true, age: time.Hour, statuses: []string{"sent"}})

	sweep, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sweep.Rollback(ctx) }()
	q := New(sweep)
	sends, err := q.TakeSendToMask(ctx, []uuid.UUID{})
	if err != nil || len(sends) != 1 || sends[0].Sensitive {
		t.Fatalf("sends %+v err %v", sends, err)
	}
	if err := q.MarkSendNotDueForMasking(ctx, task); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.Conn.Exec(ctx, `UPDATE templates SET content_sensitive = true WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the template's write did not wait for the sweep (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := sweep.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the template's write never came back")
	}
	var due, sensitive bool
	if err := db.Conn.QueryRow(ctx, `SELECT content_mask_due, mail_task_content_sensitive(id) FROM mail_tasks WHERE id = $1`, task).Scan(&due, &sensitive); err != nil {
		t.Fatal(err)
	}
	if !sensitive || !due {
		t.Fatalf("after both: sensitive %v, due %v; want a sensitive send due to be masked", sensitive, due)
	}
}

// The record of who opened a masked mail is kept retentionDays and then
// deleted, a batch at a time (Yusuf, 2026-10-14: two years by default).
func TestSentContentViewsAreDeletedPastTheirRetention(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	yes := true
	task := seedSweepSend(t, db, sweepSend{sensitive: &yes, age: time.Hour, statuses: []string{"sent"}})
	for _, age := range []string{"800 days", "731 days", "729 days", "1 day"} {
		if _, err := db.Conn.Exec(ctx, `
			INSERT INTO sent_content_views (task_id, queue_item_id, actor_sub, actor_name, masked_by, created_at)
			VALUES ($1, gen_random_uuid(), 's', 'Ayşe', 'variables', now() - $2::interval)`, task, age); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := db.DeleteSentContentViews(ctx, 730, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d views, want the two older than 730 days", deleted)
	}
	var left int
	if err := db.Conn.QueryRow(ctx,
		`SELECT count(*) FROM sent_content_views WHERE created_at > now() - interval '730 days'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	var all int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM sent_content_views`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if left != 2 || all != 2 {
		t.Errorf("left %d of %d views, want the two younger than 730 days", left, all)
	}
}

// A send that is not kept loses its rows' unsubscribe links with their mail:
// emptied at once (ClearSendContent) or past the masking deadline.
func TestASendNotKeptLosesItsUnsubscribeLinks(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	yes := true
	emptied := seedSweepSend(t, db, sweepSend{sensitive: &yes, age: time.Hour, statuses: []string{"sent", "sent"}})
	if err := db.ClearSendContent(ctx, emptied); err != nil {
		t.Fatal(err)
	}
	if state := sweptStateOf(t, db, emptied); !state.cleared || state.content != 0 {
		t.Errorf("ClearSendContent: %+v", state)
	}

	late := seedSweepSend(t, db, sweepSend{sensitive: &yes, age: time.Hour, statuses: []string{"sent"}})
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_tasks SET content_mask_failing_since = now() - INTERVAL '2 days' WHERE id = $1`, late); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearSendsPastMaskDeadline(ctx, ClearSendsPastMaskDeadlineParams{DeadlineSeconds: 3600, BatchSize: 10}); err != nil {
		t.Fatal(err)
	}
	if state := sweptStateOf(t, db, late); !state.cleared || state.content != 0 {
		t.Errorf("past the masking deadline: %+v", state)
	}
}
