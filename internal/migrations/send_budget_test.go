package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migrations of the send budget: the windows every process counts its
// sends in, and the indexes that give each lane its own pending rows.
const (
	mailSendWindows        = uint(20261012130000)
	mailQueuePendingSystem = uint(20261012130100)
	mailQueuePendingBulk   = uint(20261012130200)
)

// Up makes the windows and builds the lane indexes concurrently, which
// golang-migrate can do only outside a transaction; an image from before them
// queues as it did. Down drops them all and leaves the queue.
func TestSendBudgetMigrationsGoUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailQueuePriority); err != nil {
		t.Fatalf("migrate to %d: %v", mailQueuePriority, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000003', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, priority)
		SELECT '40000000-0000-4000-8000-000000000003', 'Alıcı ' || n, 'alici' || n || '@example.com', 'Konu', 'Gövde', (n % 2)::smallint
		FROM generate_series(1, 2000) n;`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailSendWindows); err != nil {
		t.Fatalf("up to the windows: %v", err)
	}
	for _, version := range []uint{mailQueuePendingSystem, mailQueuePendingBulk} {
		if err := runner.Migrate(version); err != nil {
			t.Fatalf("up to %d (CREATE INDEX CONCURRENTLY outside a transaction): %v", version, err)
		}
	}
	var valid int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname IN ('idx_mail_queue_pending_system', 'idx_mail_queue_pending_bulk') AND i.indisvalid`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 2 {
		t.Fatalf("%d valid lane indexes, want 2", valid)
	}

	// What an image from before the budget writes: it names its columns.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html)
		VALUES ('40000000-0000-4000-8000-000000000003', 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', NULL);`); err != nil {
		t.Fatalf("an old image's insert: %v", err)
	}
	for statement, want := range map[string]string{
		`INSERT INTO mail_send_windows VALUES (60, now(), 2, 1)`:  "23514",
		`INSERT INTO mail_send_windows VALUES (60, now(), 0, -1)`: "23514",
		`INSERT INTO mail_send_windows VALUES (0, now(), 0, 1)`:   "23514",
	} {
		if _, err := database.Pool.Exec(ctx, statement); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want SQLSTATE %s", statement, err, want)
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_send_windows VALUES (60, date_trunc('minute', now()), 0, 3);
		INSERT INTO mail_send_windows VALUES (60, date_trunc('minute', now()), 0, 4)
		ON CONFLICT (span_seconds, starts_at, lane) DO UPDATE SET taken = mail_send_windows.taken + EXCLUDED.taken;`); err != nil {
		t.Fatalf("counting in a window: %v", err)
	}

	// That each lane's take reads its index is tested on the queries sqlc
	// generated (internal/database, TestEachLaneTakeReadsItsOwnIndex).

	if err := runner.Migrate(mailQueuePriority); err != nil {
		t.Fatalf("down: %v", err)
	}
	var table, system, bulk bool
	var queued int
	if err := database.Pool.QueryRow(ctx, `
		SELECT to_regclass('mail_send_windows') IS NOT NULL,
		       to_regclass('idx_mail_queue_pending_system') IS NOT NULL,
		       to_regclass('idx_mail_queue_pending_bulk') IS NOT NULL,
		       (SELECT count(*) FROM mail_queue)`).Scan(&table, &system, &bulk, &queued); err != nil {
		t.Fatal(err)
	}
	if table || system || bulk || queued != 2001 {
		t.Fatalf("after down: windows table %v, lane indexes %v %v, %d rows queued; want none and 2001", table, system, bulk, queued)
	}
	if err := runner.Migrate(mailQueuePendingBulk); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
