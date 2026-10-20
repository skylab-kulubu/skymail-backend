package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that applies the suppression list when a send is queued.
const enforceSuppressions = uint(20261021090100)

// Up: a suppressed recipient (error 'suppressed: …') is skipped in its send's
// status, as a reserved_domain one; the flag and the removal record exist.
// Down gives back the status where it fails the send, and drops both.
func TestSuppressedRecipientsAreSkippedInTheSendStatus(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(bounces); err != nil {
		t.Fatalf("migrate to %d: %v", bounces, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES
		    ('70000000-0000-4000-8000-000000000001', 'sub', '{}'),
		    ('70000000-0000-4000-8000-000000000002', 'sub', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error) VALUES
		    ('70000000-0000-4000-8000-000000000001', 'A', 'a@yildizskylab.com', 'K', 'G', 'sent', NULL),
		    ('70000000-0000-4000-8000-000000000001', 'S', 'yok@yildizskylab.com', '', '', 'failed', 'suppressed: dsn 5.1.10'),
		    ('70000000-0000-4000-8000-000000000002', 'S', 'yok@yildizskylab.com', '', '', 'failed', 'suppressed: smtp RCPT TO 550 5.1.1')`); err != nil {
		t.Fatal(err)
	}
	statuses := func(when string, first, second string) {
		t.Helper()
		var a, b string
		if err := database.Pool.QueryRow(ctx, `
			SELECT mail_task_status('70000000-0000-4000-8000-000000000001'), mail_task_status('70000000-0000-4000-8000-000000000002')`).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if a != first || b != second {
			t.Errorf("%s: statuses %q %q, want %q %q", when, a, b, first, second)
		}
	}
	objects := func() (columns, tables int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM information_schema.columns WHERE table_name = 'mail_queue' AND column_name = 'suppressed_but_sent'),
			       (SELECT count(*) FROM information_schema.tables WHERE table_name = 'mail_suppression_removals')`).Scan(&columns, &tables); err != nil {
			t.Fatal(err)
		}
		return columns, tables
	}
	statuses("before", "failed", "failed")

	if err := runner.Migrate(enforceSuppressions); err != nil {
		t.Fatalf("up: %v", err)
	}
	// One recipient sent, one suppressed: sent. Only a suppressed one:
	// nothing went out, failed.
	statuses("after up", "sent", "failed")
	if columns, tables := objects(); columns != 1 || tables != 1 {
		t.Errorf("after up: flag %d, removals %d; want 1 and 1", columns, tables)
	}
	var flagged int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE suppressed_but_sent`).Scan(&flagged); err != nil || flagged != 0 {
		t.Errorf("rows flagged by the migration = %d (%v), want 0", flagged, err)
	}

	if err := runner.Migrate(bounces); err != nil {
		t.Fatalf("down: %v", err)
	}
	statuses("after down", "failed", "failed")
	if columns, tables := objects(); columns != 0 || tables != 0 {
		t.Errorf("after down: flag %d, removals %d; want none", columns, tables)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	statuses("after up again", "sent", "failed")
}
