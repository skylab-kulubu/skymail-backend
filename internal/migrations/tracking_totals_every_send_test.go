package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that lets every send keep its counts without people (KVKK
// notice s6; hub ticket kvkk-s6-yayin-oncesi 13).
const trackingTotalsEverySend = uint(20261023180000)

// Up: counts of fewer than 10 tracked recipients are kept. Down deletes
// them, an image from before never would, and the CHECK is back.
func TestEverySendKeepsItsTrackingTotals(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(sendPurpose); err != nil {
		t.Fatalf("migrate to %d: %v", sendPurpose, err)
	}
	var small, big string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables, content_sensitive_at_send) VALUES ('sub', '{}', 'false') RETURNING id`).Scan(&small); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables, content_sensitive_at_send) VALUES ('sub', '{}', 'false') RETURNING id`).Scan(&big); err != nil {
		t.Fatal(err)
	}
	const totals = `
		INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers)
		VALUES ($1, $2, 0, 0, 0)`
	if _, err := database.Pool.Exec(ctx, totals, small, 9); err == nil {
		t.Fatal("before up: totals of 9 kept")
	}

	if err := runner.Migrate(trackingTotalsEverySend); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, n := range []int{1, 9} {
		if _, err := database.Pool.Exec(ctx, `DELETE FROM mail_task_tracking_totals WHERE task_id = $1`, small); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(ctx, totals, small, n); err != nil {
			t.Errorf("after up: totals of %d: %v", n, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, totals, big, 10); err != nil {
		t.Fatalf("after up: totals of 10: %v", err)
	}

	if err := runner.Migrate(sendPurpose); err != nil {
		t.Fatalf("down: %v", err)
	}
	var kept []string
	rows, err := database.Pool.Query(ctx, `SELECT task_id::text FROM mail_task_tracking_totals ORDER BY task_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, id)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(kept) != 1 || kept[0] != big {
		t.Errorf("after down: totals of %v, want only the send of 10 (%s)", kept, big)
	}
	if _, err := database.Pool.Exec(ctx, totals, small, 9); err == nil {
		t.Error("after down: totals of 9 kept")
	}
}
