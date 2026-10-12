package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that gives each queue row a priority: the system lane (1,
// single mail such as a login code) or the bulk lane (0, a list or a group).
const mailQueuePriority = uint(20261012120000)

// Up gives every row already queued, and every row an image from before the
// priority writes, the system lane: nothing that was sent first yesterday
// waits behind a bulk send today. Down drops the column and leaves the rows.
func TestMailQueuePriorityMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailQueueClaim); err != nil {
		t.Fatalf("migrate to %d: %v", mailQueueClaim, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000002', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body)
		VALUES ('40000000-0000-4000-8000-000000000002', 'Alıcı', 'alici@example.com', 'Konu', 'Gövde');`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailQueuePriority); err != nil {
		t.Fatalf("up: %v", err)
	}
	// What an image from before the priority writes: it names its columns.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html)
		VALUES ('40000000-0000-4000-8000-000000000002', 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', NULL);`); err != nil {
		t.Fatalf("an old image's insert: %v", err)
	}
	var system, total int
	if err := database.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE priority = 1), count(*) FROM mail_queue`).Scan(&system, &total); err != nil {
		t.Fatal(err)
	}
	if system != 2 || total != 2 {
		t.Fatalf("%d of %d rows in the system lane after up, want both", system, total)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE mail_queue SET priority = NULL`); err == nil {
		t.Fatal("priority took NULL; it must always name a lane")
	}

	if err := runner.Migrate(mailQueueClaim); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, rows int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE table_name = 'mail_queue' AND column_name = 'priority'),
		       (SELECT count(*) FROM mail_queue)`).Scan(&columns, &rows); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || rows != 2 {
		t.Fatalf("after down: %d priority columns, %d rows; want 0 and 2", columns, rows)
	}
}
