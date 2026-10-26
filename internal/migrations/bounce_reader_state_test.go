package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that adds the NDR reader's state, and the one before it.
const (
	beforeBounceReaderState = uint(20261024200000)
	bounceReaderState       = uint(20261025120000)
)

// Up adds mail_bounce_reader_state and nothing else; it takes a mailbox in
// lower case, a mode of the reader and an http(s) delta link only. Down
// drops it, and up again works.
func TestTheBounceReaderStateGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeBounceReaderState); err != nil {
		t.Fatalf("migrate to %d: %v", beforeBounceReaderState, err)
	}
	exists := func() bool {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'mail_bounce_reader_state'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if exists() {
		t.Fatal("the table exists before the migration")
	}
	if err := runner.Migrate(bounceReaderState); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !exists() {
		t.Fatal("up did not add the table")
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_bounce_reader_state (mailbox, mode, delta_link) VALUES
		    ('hesap@bildirim.example.org', 'on', 'https://graph.microsoft.com/v1.0/users/x/mailFolders/inbox/messages/delta?$deltatoken=a'),
		    ('hesap@bildirim.example.org', 'dry-run', NULL)`); err != nil {
		t.Fatalf("a valid row: %v", err)
	}
	for _, bad := range []string{
		`INSERT INTO mail_bounce_reader_state (mailbox, mode) VALUES ('Hesap@bildirim.example.org', 'on')`,
		`INSERT INTO mail_bounce_reader_state (mailbox, mode) VALUES ('bilgi@bildirim.example.org', 'apply')`,
		`INSERT INTO mail_bounce_reader_state (mailbox, mode, delta_link) VALUES ('bilgi@bildirim.example.org', 'on', 'file:///etc/passwd')`,
		`INSERT INTO mail_bounce_reader_state (mailbox, mode, last_error) VALUES ('bilgi@bildirim.example.org', 'on', repeat('x', 201))`,
	} {
		if _, err := database.Pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if err := runner.Migrate(beforeBounceReaderState); err != nil {
		t.Fatalf("down: %v", err)
	}
	if exists() {
		t.Fatal("down did not drop the table")
	}
	if err := runner.Migrate(bounceReaderState); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
