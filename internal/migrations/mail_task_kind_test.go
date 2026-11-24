package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration of the send list's kind and search.
const mailTaskKind = uint(20261124200000)

// Up adds sent_by_service, mail_task_kind() and skymail_search_fold(); a send
// from before keeps its kind by its lane; down drops them; up again works.
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
	// skymail_search_fold() is inlined: a SQL function with a SET is not.
	var config []string
	if err := database.Pool.QueryRow(ctx, `SELECT COALESCE(proconfig, '{}') FROM pg_proc WHERE proname = 'skymail_search_fold'`).Scan(&config); err != nil || len(config) != 0 {
		t.Errorf("skymail_search_fold settings = %v %v; want none", config, err)
	}
	rows, err := database.Pool.Query(ctx, `EXPLAIN (VERBOSE, COSTS OFF) SELECT skymail_search_fold(name) FROM templates`)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if plan := strings.Join(lines, "\n"); strings.Contains(plan, "skymail_search_fold") || !strings.Contains(plan, "translate") {
		t.Errorf("not inlined:\n%s", plan)
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
	var folded string
	if err := database.Pool.QueryRow(ctx, `SELECT skymail_search_fold('IŞIK İŞ Ğ Ü Ö Ç ı')`).Scan(&folded); err != nil || folded != "işik iş ğ ü ö ç i" {
		t.Errorf("fold = %q %v", folded, err)
	}

	if err := runner.Migrate(recipientRetention); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns WHERE table_name = 'mail_tasks' AND column_name = 'sent_by_service')
		     + (SELECT count(*) FROM pg_proc WHERE proname IN ('mail_task_kind', 'skymail_search_fold'))`).Scan(&left); err != nil || left != 0 {
		t.Errorf("after down: %d left %v", left, err)
	}
	if err := runner.Migrate(mailTaskKind); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
