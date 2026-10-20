package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that adds mail_bounces and mail_suppressions, and the one
// before it.
const (
	beforeBounces = uint(20261020150000)
	bounces       = uint(20261021090000)
)

// Up adds the two tables and nothing else; a bounce goes with its queue row,
// a suppression only loses its last row. Down drops them, and up again works.
func TestBounceTablesGoUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeBounces); err != nil {
		t.Fatalf("migrate to %d: %v", beforeBounces, err)
	}
	tables := func() int {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name IN ('mail_bounces', 'mail_suppressions')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := tables(); n != 0 {
		t.Fatalf("before: %d tables", n)
	}

	if err := runner.Migrate(bounces); err != nil {
		t.Fatalf("up: %v", err)
	}
	if n := tables(); n != 2 {
		t.Fatalf("after up: %d tables, want 2", n)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES ('70000000-0000-4000-8000-000000000001', 'sub', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status) VALUES
		    ('71000000-0000-4000-8000-000000000001', '70000000-0000-4000-8000-000000000001', 'A', 'a@yildizskylab.com', 'K', 'G', 'sent');
		INSERT INTO mail_bounces (queue_id, source, kind, action, status, smtp_code) VALUES
		    ('71000000-0000-4000-8000-000000000001', 'smtp', 'hard', 'failed', '5.1.1', 550);
		INSERT INTO mail_suppressions (address, source, reason_code, last_queue_id) VALUES
		    ('a@yildizskylab.com', 'smtp', 'RCPT TO 550 5.1.1', '71000000-0000-4000-8000-000000000001');
		DELETE FROM mail_queue WHERE id = '71000000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	var bounceRows, suppressed int
	var lastQueue *string
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM mail_bounces), (SELECT count(*) FROM mail_suppressions),
		       (SELECT last_queue_id::text FROM mail_suppressions)`).Scan(&bounceRows, &suppressed, &lastQueue); err != nil {
		t.Fatal(err)
	}
	if bounceRows != 0 || suppressed != 1 || lastQueue != nil {
		t.Errorf("after the queue row went: %d bounces, %d suppressions (last row %v); want 0, 1 and none", bounceRows, suppressed, lastQueue)
	}

	if err := runner.Migrate(beforeBounces); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := tables(); n != 0 {
		t.Fatalf("after down: %d tables", n)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := tables(); n != 2 {
		t.Fatalf("after up again: %d tables, want 2", n)
	}
}
