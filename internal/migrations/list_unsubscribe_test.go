package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps a queue row's one-click unsubscribe link.
const listUnsubscribe = uint(20261021130000)

// Up: mail_queue.list_unsubscribe_url, null on the rows already there, an
// https link of at most 960 characters on a new one. Down drops it, and up
// again comes back.
func TestQueueRowsKeepAnUnsubscribeLink(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(enforceSuppressions); err != nil {
		t.Fatalf("migrate to %d: %v", enforceSuppressions, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES ('71000000-0000-4000-8000-000000000001', 'sub', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body)
		VALUES ('71000000-0000-4000-8000-000000000001', 'A', 'a@yildizskylab.com', 'K', 'G')`); err != nil {
		t.Fatal(err)
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns WHERE table_name = 'mail_queue' AND column_name = 'list_unsubscribe_url'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := runner.Migrate(listUnsubscribe); err != nil {
		t.Fatalf("up: %v", err)
	}
	var nulls int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE list_unsubscribe_url IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("rows without a link = %d (%v), want the one already there", nulls, err)
	}
	insert := func(link string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, list_unsubscribe_url)
			VALUES ('71000000-0000-4000-8000-000000000001', 'B', 'b@yildizskylab.com', 'K', 'G', $1)`, link)
		return err
	}
	if err := insert("https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=AQ"); err != nil {
		t.Errorf("an https link: %v", err)
	}
	for _, link := range []string{"http://api.yildizskylab.com/u?t=AQ", "mailto:x@example.com", "https://x/" + strings.Repeat("a", 951)} {
		if err := insert(link); err == nil || !strings.Contains(err.Error(), "mail_queue_list_unsubscribe_url_check") {
			t.Errorf("%.40q: %v, want the check", link, err)
		}
	}

	if err := runner.Migrate(enforceSuppressions); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := columns(); n != 0 {
		t.Errorf("after down: %d columns", n)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := columns(); n != 1 {
		t.Errorf("after up again: %d columns", n)
	}
}
