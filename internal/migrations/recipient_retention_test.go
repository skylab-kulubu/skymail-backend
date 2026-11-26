package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

// The migrations before and of the recipient retention run.
const (
	waitlistLocked     = uint(20261124120000)
	recipientRetention = uint(20261124180000)
)

// Up lets an emptied Mail onayı or Forms row through the address CHECK and
// the once-only index, and only an emptied one; down gives every emptied row
// a stand-in address so the old CHECKs and indexes hold again, drops the
// receipts and the columns; up again works.
func TestRecipientRetentionMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(recipientRetention); err != nil {
		t.Fatalf("up: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, body_variables, created_at, submitted_at, deadline_at, updated_at, state)
		VALUES ('72000000-0000-4000-8000-000000000001', 'sub', gen_random_uuid(), gen_random_uuid(), '{}', now(), now(), now() + interval '7 days', now(), 'rejected');
		INSERT INTO mail_approval_recipients (approval_id, position, email, full_name, cleared_at) VALUES
		    ('72000000-0000-4000-8000-000000000001', 1, '', '', now()),
		    ('72000000-0000-4000-8000-000000000001', 2, '', '', now()),
		    ('72000000-0000-4000-8000-000000000001', 3, 'kalan@example.com', 'Kalan', NULL);
		INSERT INTO mail_form_sources (id, owner_sub, form_id, form_title, prepared_at, expires_at, response_count, recipient_count,
		                               skipped_count, duplicate_count, snapshot_hash)
		VALUES ('76000000-0000-4000-8000-000000000001', 'sub', gen_random_uuid(), 'Form', now(), now() + interval '1 day', 3, 3, 0, 0,
		        decode(repeat('00', 32), 'hex'));
		INSERT INTO mail_form_source_recipients (source_id, position, email, full_name, cleared_at) VALUES
		    ('76000000-0000-4000-8000-000000000001', 1, '', '', now()),
		    ('76000000-0000-4000-8000-000000000001', 2, '', '', now()),
		    ('76000000-0000-4000-8000-000000000001', 3, 'kalan@example.com', 'Kalan', NULL)`); err != nil {
		t.Fatalf("emptied rows refused: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO mail_approval_recipients (approval_id, position, email, full_name) VALUES ('72000000-0000-4000-8000-000000000001', 4, '', '')`); err == nil {
		t.Error("an empty address without cleared_at was written")
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE mail_form_source_recipients SET cleared_at = NULL WHERE position = 1`); err == nil {
		t.Error("a Forms row with an empty address and no cleared_at was written")
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO recipient_retention_runs (mode) VALUES ('dry-run')`); err != nil {
		t.Fatalf("a receipt refused: %v", err)
	}

	if err := runner.Migrate(waitlistLocked); err != nil {
		t.Fatalf("down: %v", err)
	}
	var stand, left int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE email LIKE 'saklama-suresi-doldu-%@invalid'), count(*) FILTER (WHERE email = '') FROM mail_approval_recipients`).Scan(&stand, &left); err != nil {
		t.Fatal(err)
	}
	if stand != 2 || left != 0 {
		t.Errorf("down left %d stand-ins and %d empty addresses, want 2 and 0", stand, left)
	}
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE email LIKE 'saklama-suresi-doldu-%@invalid'), count(*) FILTER (WHERE email = '') FROM mail_form_source_recipients`).Scan(&stand, &left); err != nil {
		t.Fatal(err)
	}
	if stand != 2 || left != 0 {
		t.Errorf("down left %d Forms stand-ins and %d empty addresses, want 2 and 0", stand, left)
	}
	var table, column bool
	if err := database.Pool.QueryRow(ctx, `SELECT to_regclass('recipient_retention_runs') IS NOT NULL,
	        EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'mail_tasks' AND column_name = 'recipient_retention_at')`).Scan(&table, &column); err != nil {
		t.Fatal(err)
	}
	if table || column {
		t.Errorf("down left the receipts table %v, the send column %v", table, column)
	}
	if err := runner.Migrate(recipientRetention); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
