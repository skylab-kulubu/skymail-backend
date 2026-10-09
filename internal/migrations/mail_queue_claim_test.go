package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that leases a queue row to the process that took it.
const mailQueueClaim = uint(20261009120000)

// Up adds the two claim columns, empty for the rows already queued, and an
// insert that names only the old columns (an image from before them) still
// works. Down drops them and leaves the rows.
func TestMailQueueClaimMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(accountErasureReceipts); err != nil {
		t.Fatalf("migrate to %d: %v", accountErasureReceipts, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000001', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status)
		VALUES ('40000000-0000-4000-8000-000000000001', 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', 'processing');`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailQueueClaim); err != nil {
		t.Fatalf("up: %v", err)
	}
	var unclaimed int
	if err := database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_queue WHERE claimed_at IS NULL AND claimed_by IS NULL AND status = 'processing'`).
		Scan(&unclaimed); err != nil {
		t.Fatal(err)
	}
	if unclaimed != 1 {
		t.Fatalf("%d processing rows without a claim after up, want the one queued before", unclaimed)
	}
	// What an image from before the lease writes and reads.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body)
		VALUES ('40000000-0000-4000-8000-000000000001', 'Alıcı', 'alici@example.com', 'Konu', 'Gövde');
		UPDATE mail_queue SET status = 'processing' WHERE status = 'pending';`); err != nil {
		t.Fatalf("an old image's insert and claim: %v", err)
	}

	if err := runner.Migrate(accountErasureReceipts); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, rows int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE table_name = 'mail_queue' AND column_name IN ('claimed_at', 'claimed_by')),
		       (SELECT count(*) FROM mail_queue)`).Scan(&columns, &rows); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || rows != 2 {
		t.Fatalf("after down: %d claim columns, %d rows; want 0 and 2", columns, rows)
	}
}
