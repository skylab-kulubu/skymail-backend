package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that gives each queue row its channel (email, the only one
// yet) and the recipient's Keycloak subject when it is known.
const (
	maskDeadlineAndSensitiveVariables = uint(20261014120000)
	mailQueueChannel                  = uint(20261014150000)
)

// What an image from before the channel sends the database, as its sqlc
// generated it: it names its columns, so it neither reads nor writes the new
// ones.
const (
	oldImageQueueInsert = `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, priority)
		VALUES ('40000000-0000-4000-8000-000000000003', 'Eski', 'eski@example.com', 'Konu', 'Gövde', NULL, 1)`
	oldImageSystemClaim = `
		UPDATE mail_queue
		SET status     = 'processing',
		    claimed_at = NOW(),
		    claimed_by = $1::text
		WHERE id IN (SELECT id
		             FROM mail_queue
		             WHERE status = 'pending'
		               AND priority > 0
		               AND next_attempt_at <= NOW()
		             ORDER BY next_attempt_at, created_at
		             LIMIT $2::int FOR UPDATE SKIP LOCKED)
		RETURNING id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status, error, created_at, attempts, next_attempt_at, claimed_at, claimed_by, priority`
)

// Up makes every row already queued, and every row an image from before it
// writes, an e-mail with no subject known; a channel other than email, or
// none, is refused; an old image's claim reads the queue as before. Down
// drops both columns and the check and leaves the rows.
func TestMailQueueChannelMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(maskDeadlineAndSensitiveVariables); err != nil {
		t.Fatalf("migrate to %d: %v", maskDeadlineAndSensitiveVariables, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000003', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, oldImageQueueInsert); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailQueueChannel); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, oldImageQueueInsert); err != nil {
		t.Fatalf("an old image's insert: %v", err)
	}
	var email, withoutSubject, total int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE channel = 'email'),
		       count(*) FILTER (WHERE recipient_sub IS NULL),
		       count(*)
		FROM mail_queue`).Scan(&email, &withoutSubject, &total); err != nil {
		t.Fatal(err)
	}
	if total != 2 || email != 2 || withoutSubject != 2 {
		t.Fatalf("after up: %d of %d rows e-mail, %d without a subject; want all of them both", email, total, withoutSubject)
	}

	rows, err := database.Pool.Query(ctx, oldImageSystemClaim, "old-image", 10)
	if err != nil {
		t.Fatalf("an old image's claim: %v", err)
	}
	claimed := 0
	for rows.Next() {
		claimed++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("an old image's claim: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("an old image claimed %d rows, want both", claimed)
	}

	insert := func(channel any, sub any) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, channel, recipient_sub)
			VALUES ('40000000-0000-4000-8000-000000000003', 'Yeni', 'yeni@example.com', 'Konu', 'Gövde', $1, $2)`,
			channel, sub)
		return err
	}
	if err := insert("email", "7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10"); err != nil {
		t.Fatalf("an e-mail to a known person: %v", err)
	}
	for _, channel := range []any{"sms", "push", "inbox", ""} {
		var pgErr *pgconn.PgError
		if err := insert(channel, nil); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("channel %q: %v, want a check violation: email is the only channel yet", channel, err)
		}
	}
	if err := insert(nil, nil); err == nil {
		t.Error("channel took NULL; it must always name one")
	}

	if err := runner.Migrate(maskDeadlineAndSensitiveVariables); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, checks, left int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE table_name = 'mail_queue' AND column_name IN ('channel', 'recipient_sub')),
		       (SELECT count(*) FROM pg_constraint WHERE conname = 'mail_queue_channel_check'),
		       (SELECT count(*) FROM mail_queue)`).Scan(&columns, &checks, &left); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || checks != 0 || left != 3 {
		t.Fatalf("after down: %d columns, %d checks, %d rows; want 0, 0 and 3", columns, checks, left)
	}
}
