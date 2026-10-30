package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration of the bulk lane's guards (hub ticket duyuru-haber 05, G3 and
// G4).
const bulkTaskControls = uint(20261027120000)

// Up: mail_queue.throttles, 0 on a row from before. A paused send's rows are
// parked (next_attempt_at 2999-01-01); down puts them back due, since nothing
// could resume them without the columns, and drops the columns; up again.
func TestBulkTaskControlsDownPutsParkedRowsBackDue(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(preferenceCheckedAt); err != nil {
		t.Fatalf("migrate to %d: %v", preferenceCheckedAt, err)
	}
	var task string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('sub', '{}') RETURNING id::text`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority) VALUES
		    ($1, 'Ayşe', 'ayse@example.com', 'x', 'x', 'pending', 0),
		    ($1, 'Barış', 'baris@example.com', 'x', 'x', 'pending', 0),
		    ($1, 'Cem', 'cem@example.com', 'x', 'x', 'sent', 0)`, task); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(bulkTaskControls); err != nil {
		t.Fatalf("up: %v", err)
	}
	var throttles int
	if err := database.Pool.QueryRow(ctx, `SELECT sum(throttles) FROM mail_queue WHERE task_id = $1`, task).Scan(&throttles); err != nil || throttles != 0 {
		t.Fatalf("rows from before: throttles %d, %v; want 0", throttles, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		UPDATE mail_tasks SET paused_at = now(), paused_reason = 'operator' WHERE id = $1;`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		UPDATE mail_queue SET next_attempt_at = '2999-01-01' WHERE task_id = $1 AND status = 'pending'`, task); err != nil {
		t.Fatal(err)
	}

	if err := runner.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var parked, due int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE next_attempt_at >= '2999-01-01'),
		       count(*) FILTER (WHERE next_attempt_at <= now())
		FROM mail_queue WHERE task_id = $1 AND status = 'pending'`, task).Scan(&parked, &due); err != nil {
		t.Fatal(err)
	}
	if parked != 0 || due != 2 {
		t.Fatalf("after down: %d pending rows still parked, %d due; want 0 and 2", parked, due)
	}
	var columns int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'mail_queue' AND column_name = 'throttles')
		   OR (table_name = 'mail_tasks' AND column_name IN ('paused_at', 'cancelled_at', 'send_failures'))`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("after down: %d of the columns kept, %v", columns, err)
	}
	if err := runner.Migrate(bulkTaskControls); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
