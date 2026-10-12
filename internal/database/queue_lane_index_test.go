package database

import (
	"context"
	"strings"
	"testing"
)

// Each lane's take, the query as sqlc generated it from query.sql, reads its
// own pending index in the order it takes rows (no sort): a lane's turn never
// walks the other lane's rows (idx_mail_queue_pending_system,
// idx_mail_queue_pending_bulk).
func TestEachLaneTakeReadsItsOwnIndex(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	queueOf(t, db, 200)
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET priority = 0 WHERE random() < 0.5`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Sequential scans off: on a table this small the planner would read it
	// whole; what matters is which index it picks when it uses one.
	if _, err := conn.Exec(ctx, `ANALYZE mail_queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET enable_seqscan`) }()
	for query, index := range map[string]string{
		processSystemQueueItems: "idx_mail_queue_pending_system",
		processBulkQueueItems:   "idx_mail_queue_pending_bulk",
	} {
		rows, err := conn.Query(ctx, "EXPLAIN "+query, "skymail-a-1", 3)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "Index Scan using "+index) || strings.Contains(plan.String(), "Sort") {
			t.Errorf("the take does not read %s in order:\n%s", index, plan.String())
		}
	}
}
