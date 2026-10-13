package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migrations that let a send's content be shown: which templates hide it,
// when a send's content was emptied, and the index the sweep reads.
const (
	sentMailContentControls = uint(20261012180000)
	mailTasksContentKept    = uint(20261012180100)
)

// Up marks the system templates that carry a code or a one-time link
// sensitive, leaves the others to the kind of send, keeps every send's content
// and lets an image from before it write as it did. Down drops both columns,
// both functions and the index.
func TestSentMailContentMigrationsGoUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailQueuePendingBulk); err != nil {
		t.Fatalf("migrate to %d: %v", mailQueuePendingBulk, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, system)
		VALUES ('keycloak.reset-password', 'Parola', 'Konu', '<p>{{.link}}</p>', '{{.link}}', '', true),
		       ('account.security-alert', 'Güvenlik', 'Konu', '<p>x</p>', 'x', '', true),
		       ('core.contact-consent-confirm', 'Onay', 'Konu', '<p>x</p>', 'x', '', true),
		       ('event.ticket-created', 'Bilet', 'Konu', '<p>x</p>', 'x', '', false),
		       ('core.welcome', 'Hoş geldin', 'Konu', '<p>x</p>', 'x', '', true),
		       ('free.basic', 'Serbest', 'Konu', '<p>x</p>', 'x', '', false),
		       ('mail.approval-resolved', 'Onay', 'Konu', '<p>x</p>', 'x', '', false),
		       ('forms.form-copy', 'Form kopyası', 'Konu', '<p>x</p>', 'x', '', true);
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000003', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailTasksContentKept); err != nil {
		t.Fatalf("up: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `SELECT key, content_sensitive FROM templates ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*bool{}
	for rows.Next() {
		var key string
		var sensitive *bool
		if err := rows.Scan(&key, &sensitive); err != nil {
			t.Fatal(err)
		}
		got[key] = sensitive
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	for key, want := range map[string]bool{
		"keycloak.reset-password": true, "account.security-alert": true, "core.contact-consent-confirm": true, "event.ticket-created": true,
	} {
		if got[key] == nil || *got[key] != want {
			t.Errorf("%s content_sensitive = %v, want %v", key, got[key], want)
		}
	}
	// Shown by the seed, marked so before it runs: the first sweep must not
	// take their single mails for sensitive.
	for _, key := range []string{"core.welcome", "free.basic", "mail.approval-resolved"} {
		if got[key] == nil || *got[key] {
			t.Errorf("%s content_sensitive = %v, want false", key, got[key])
		}
	}
	if got["forms.form-copy"] != nil {
		t.Errorf("forms.form-copy content_sensitive = %v, want NULL: left to the kind of send", *got["forms.form-copy"])
	}
	var cleared, valid bool
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT content_cleared_at IS NOT NULL FROM mail_tasks WHERE id = '40000000-0000-4000-8000-000000000003'),
		       (SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_mail_tasks_content_kept'::regclass)`).Scan(&cleared, &valid); err != nil {
		t.Fatal(err)
	}
	if cleared || !valid {
		t.Fatalf("after up: a send's content cleared %v, index valid %v; want kept and a valid index", cleared, valid)
	}
	// What an image from before writes: it names its columns.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content) VALUES ('Eski', 'K', '<p>x</p>', 'x', '');
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables) VALUES ('sub', NULL, NULL, '{}');`); err != nil {
		t.Fatalf("an old image's insert: %v", err)
	}

	if err := runner.Migrate(mailQueuePendingBulk); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, functions, indexes int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE (table_name, column_name) IN (('templates', 'content_sensitive'), ('mail_tasks', 'content_cleared_at'))),
		       (SELECT count(*) FROM pg_proc WHERE proname IN ('mail_task_bulk', 'mail_task_content_sensitive'))
		           + (SELECT count(*) FROM pg_tables WHERE tablename = 'template_content_sensitive_events'),
		       (SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_mail_tasks_content_kept')`).Scan(&columns, &functions, &indexes); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || functions != 0 || indexes != 0 {
		t.Fatalf("after down: %d columns, %d functions and tables, %d indexes left; want none", columns, functions, indexes)
	}
}
