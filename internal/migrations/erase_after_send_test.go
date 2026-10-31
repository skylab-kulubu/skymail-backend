package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const eraseAfterSend = uint(20261031230000)

// The flag and its trigger come and go together; down leaves the rows it
// erased as they are.
func TestEraseAfterSendMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(dropNewsAudienceIndex); err != nil {
		t.Fatalf("migrate to %d: %v", dropNewsAudienceIndex, err)
	}
	if err := runner.Migrate(eraseAfterSend); err != nil {
		t.Fatalf("up: %v", err)
	}
	var flagged bool
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('core', '{}') RETURNING erase_after_send`).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if flagged {
		t.Error("a send is erase_after_send by default")
	}
	if err := runner.Migrate(dropNewsAudienceIndex); err != nil {
		t.Fatalf("down: %v", err)
	}
	var column, trigger, function bool
	if err := database.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'mail_tasks' AND column_name = 'erase_after_send'),
		       EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'mail_queue_erase_after_send'),
		       to_regprocedure('mail_queue_erase_after_send()') IS NOT NULL`).Scan(&column, &trigger, &function); err != nil {
		t.Fatal(err)
	}
	if column || trigger || function {
		t.Errorf("down left column %v, trigger %v, function %v", column, trigger, function)
	}
}
