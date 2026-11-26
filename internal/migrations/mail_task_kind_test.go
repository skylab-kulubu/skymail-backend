package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

// The migration of the send list's kind and search.
const mailTaskKind = uint(20261124200000)

// Up adds sent_by_service, mail_task_kind() and the search fold function (its
// name and checks: skynotify_tasima_test.go); a send from before keeps its
// kind by its lane; down drops them; up again works.
func TestMailTaskKindMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(recipientRetention); err != nil {
		t.Fatalf("before: %v", err)
	}
	// Two single sends written by an image from before: one in the system
	// lane, one in the bulk lane.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES
		    ('77000000-0000-4000-8000-000000000001', 'a', '{}'),
		    ('77000000-0000-4000-8000-000000000002', 'b', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, priority) VALUES
		    ('77000000-0000-4000-8000-000000000001', 'A', 'a@example.com', 'K', 'G', 1),
		    ('77000000-0000-4000-8000-000000000002', 'B', 'b@example.com', 'K', 'G', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(mailTaskKind); err != nil {
		t.Fatalf("up: %v", err)
	}
	kinds := func() (string, string) {
		t.Helper()
		var lane, bulkLane string
		if err := database.Pool.QueryRow(ctx, `SELECT mail_task_kind('77000000-0000-4000-8000-000000000001'),
		                                              mail_task_kind('77000000-0000-4000-8000-000000000002')`).Scan(&lane, &bulkLane); err != nil {
			t.Fatal(err)
		}
		return lane, bulkLane
	}
	if lane, bulkLane := kinds(); lane != "system" || bulkLane != "single" {
		t.Errorf("sends from before: %s, %s; want system (its lane), single", lane, bulkLane)
	}
	// An erased send from before (erase_after_send, a service's only) has no
	// rows left to tell its lane: it is system all the same.
	if _, err := database.Pool.Exec(ctx, `INSERT INTO mail_tasks (id, sent_by, body_variables, erase_after_send)
		VALUES ('77000000-0000-4000-8000-000000000003', 'c', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	var erased string
	if err := database.Pool.QueryRow(ctx, `SELECT mail_task_kind('77000000-0000-4000-8000-000000000003')`).Scan(&erased); err != nil || erased != "system" {
		t.Errorf("an erased send from before: %s %v; want system", erased, err)
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM mail_tasks WHERE id = '77000000-0000-4000-8000-000000000003'`); err != nil {
		t.Fatal(err)
	}
	// Written by this image, the column decides over the lane.
	if _, err := database.Pool.Exec(ctx, `UPDATE mail_tasks SET sent_by_service = (id = '77000000-0000-4000-8000-000000000002')`); err != nil {
		t.Fatal(err)
	}
	if lane, bulkLane := kinds(); lane != "single" || bulkLane != "system" {
		t.Errorf("with sent_by_service: %s, %s; want single, system", lane, bulkLane)
	}

	if err := runner.Migrate(recipientRetention); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns WHERE table_name = 'mail_tasks' AND column_name = 'sent_by_service')
		     + (SELECT count(*) FROM pg_proc WHERE proname = 'mail_task_kind' OR proname LIKE '%search_fold')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after down: %d left %v", left, err)
	}
	if err := runner.Migrate(mailTaskKind); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
