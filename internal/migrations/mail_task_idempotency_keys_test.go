package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

const mailTaskIdempotencyKeys = uint(20261122120000)

// The keys of single mails: one per service and key, only a visible ASCII
// key and a SHA-256, gone with their send; down drops them and leaves the
// sends.
func TestMailTaskIdempotencyKeysMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailApprovalTeamIDs); err != nil {
		t.Fatalf("migrate to %d: %v", mailApprovalTeamIDs, err)
	}
	if err := runner.Migrate(mailTaskIdempotencyKeys); err != nil {
		t.Fatalf("up: %v", err)
	}
	var task string
	if err := database.Pool.QueryRow(ctx, `INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('core', '{}') RETURNING id`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	insert := func(key, digest string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_task_idempotency_keys (sent_by, idempotency_key, request_sha256, task_id, result)
			VALUES ('core', $1, $2, $3, '{}')`, key, digest, task)
		return err
	}
	if err := insert("core:event.ticket-created:1:v1", sha); err != nil {
		t.Fatalf("a key: %v", err)
	}
	for _, c := range []struct{ name, key, digest string }{
		{"the same key again", "core:event.ticket-created:1:v1", sha},
		{"an empty key", "", sha},
		{"a space", "two words", sha},
		{"a key of 256", strings.Repeat("k", 256), sha},
		{"no SHA-256", "core:2", "not-a-digest"},
	} {
		if err := insert(c.key, c.digest); err == nil {
			t.Errorf("%s was written", c.name)
		}
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM mail_tasks WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	var keys int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_task_idempotency_keys`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 0 {
		t.Errorf("%d keys outlived their send", keys)
	}

	if err := runner.Migrate(mailApprovalTeamIDs); err != nil {
		t.Fatalf("down: %v", err)
	}
	var table bool
	if err := database.Pool.QueryRow(ctx, `SELECT to_regclass('mail_task_idempotency_keys') IS NOT NULL`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table {
		t.Error("down left the table")
	}
}
