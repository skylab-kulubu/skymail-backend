package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps a list send's own event statement (KVKK s6 row
// 15; hub ticket kvkk-s6-yayin-oncesi 12, T3).
const ownEventStatement = uint(20261024200000)

// Up: a NULL column on mail_tasks and mail_approvals, the old image's rows
// NULL; the statement only on a duyuru send. Down drops it, up brings it back.
func TestListSendsKeepTheirOwnEventStatement(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(trackingTotalsEverySend); err != nil {
		t.Fatalf("migrate to %d: %v", trackingTotalsEverySend, err)
	}
	var templateID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Serbest Gönderim', 'x', '<p>x</p>', 'x', '') RETURNING id::text`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name IN ('mail_tasks', 'mail_approvals') AND column_name = 'own_event_statement'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := runner.Migrate(ownEventStatement); err != nil {
		t.Fatalf("up: %v", err)
	}
	if n := columns(); n != 2 {
		t.Fatalf("after up: %d columns, want 2", n)
	}
	var old *bool
	if err := database.Pool.QueryRow(ctx, oldImageInsertTask+`, own_event_statement`, templateID).Scan(new(string), &old); err != nil || old != nil {
		t.Errorf("the old image's send: %v %v, want NULL", old, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
		                        purpose, category, own_event_statement)
		VALUES ('sub', $1, '{}', 'unset', false, 'duyuru', 'club:duyuru', true)`, templateID); err != nil {
		t.Errorf("a duyuru send with the statement: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
		                        purpose, transactional_statement, own_event_statement)
		VALUES ('sub', $1, '{}', 'unset', false, 'islem', true, true)`, templateID); err == nil {
		t.Error("an islem send took the statement")
	}

	if err := runner.Steps(-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := columns(); n != 0 {
		t.Fatalf("after down: %d columns, want 0", n)
	}
	if err := runner.Migrate(ownEventStatement); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
