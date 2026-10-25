package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration of the bulk preference check (hub ticket abonelik-cikisi
// 09).
const preferenceCheckedAt = uint(20261025200000)

// Up: a NULL column on mail_queue, and a send whose only failed recipients
// core held back reads as its others do; 'preference: unavailable' is no
// skip. Down drops the column and the send reads failed again; up again.
func TestPreferenceHeldRecipientsAreSkipped(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(bounceReaderState); err != nil {
		t.Fatalf("migrate to %d: %v", bounceReaderState, err)
	}
	var templateID, task string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Serbest Gönderim', 'x', '<p>x</p>', 'x', '') RETURNING id::text`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested)
		VALUES ('sub', $1, '{}', 'unset', false) RETURNING id::text`, templateID).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error) VALUES
		    ($1, 'Ayşe', 'ayse@example.com', 'x', 'x', 'sent', NULL),
		    ($1, 'Barış', 'baris@example.com', 'x', 'x', 'failed', 'preference: opted_out'),
		    ($1, 'Cem', 'cem@example.com', 'x', 'x', 'failed', 'preference: inactive'),
		    ($1, 'Deniz', 'deniz@example.com', 'x', 'x', 'failed', 'preference: ambiguous')`, task); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		var s string
		if err := database.Pool.QueryRow(ctx, `SELECT mail_task_status($1)`, task).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	column := func() bool {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns WHERE table_name = 'mail_queue' AND column_name = 'preference_checked_at'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if got := status(); got != "failed" {
		t.Fatalf("before: %s, want failed", got)
	}

	if err := runner.Migrate(preferenceCheckedAt); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !column() {
		t.Fatal("after up: no preference_checked_at")
	}
	if got := status(); got != "sent" {
		t.Errorf("after up: %s, want sent: the held are skipped", got)
	}
	var checked *string
	if err := database.Pool.QueryRow(ctx, `SELECT preference_checked_at::text FROM mail_queue WHERE recipient_email = 'ayse@example.com'`).Scan(&checked); err != nil || checked != nil {
		t.Errorf("an old row's preference_checked_at: %v %v, want NULL", checked, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error)
		VALUES ($1, 'Ece', 'ece@example.com', 'x', 'x', 'failed', 'preference: unavailable')`, task); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "failed" {
		t.Errorf("undecided: %s, want failed", got)
	}

	if err := runner.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column() {
		t.Fatal("after down: preference_checked_at kept")
	}
	if err := runner.Migrate(preferenceCheckedAt); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
