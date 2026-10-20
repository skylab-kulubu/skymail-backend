package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const (
	trackingRetentionRuns        = uint(20261019120000)
	freezeContentSensitiveAtSend = uint(20261020120000)
)

// Up writes 'true' into the copy of every kept send that is sensitive today,
// the locked key and the template's value now included, and leaves the others
// as they were (hub ticket iletisim-servisi 21).
func TestFreezeContentSensitiveMigrationFillsTheSendsSensitiveToday(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(trackingRetentionRuns); err != nil {
		t.Fatalf("migrate to %d: %v", trackingRetentionRuns, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('10000000-0000-4000-8000-000000000001', 'event.ticket-created', 'Bilet', 'K', '<p>x</p>', 'x', '', false),
		       ('10000000-0000-4000-8000-000000000002', 'club.notice', 'Duyuru', 'K', '<p>x</p>', 'x', '', false),
		       ('10000000-0000-4000-8000-000000000003', 'forms.form-copy', 'Form', 'K', '<p>x</p>', 'x', '', NULL),
		       ('10000000-0000-4000-8000-000000000004', 'club.secret', 'Gizli', 'K', '<p>x</p>', 'x', '', true);
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, content_sensitive_at_send) VALUES
		    ('40000000-0000-4000-8000-000000000001', 's', '10000000-0000-4000-8000-000000000001', gen_random_uuid(), '{}', 'false'),
		    ('40000000-0000-4000-8000-000000000002', 's', '10000000-0000-4000-8000-000000000002', gen_random_uuid(), '{}', 'false'),
		    ('40000000-0000-4000-8000-000000000003', 's', '10000000-0000-4000-8000-000000000003', NULL, '{}', 'unset'),
		    ('40000000-0000-4000-8000-000000000004', 's', '10000000-0000-4000-8000-000000000003', gen_random_uuid(), '{}', 'unset'),
		    ('40000000-0000-4000-8000-000000000005', 's', '10000000-0000-4000-8000-000000000004', gen_random_uuid(), '{}', 'false'),
		    ('40000000-0000-4000-8000-000000000006', 's', NULL, NULL, '{}', 'unset');`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(freezeContentSensitiveAtSend); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := map[string]string{
		"40000000-0000-4000-8000-000000000001": "true",  // locked key, row says false
		"40000000-0000-4000-8000-000000000002": "false", // shown, to a list
		"40000000-0000-4000-8000-000000000003": "true",  // single left to its kind
		"40000000-0000-4000-8000-000000000004": "unset", // the same template to a list
		"40000000-0000-4000-8000-000000000005": "true",  // made sensitive after the send
		"40000000-0000-4000-8000-000000000006": "true",  // no template: a single mail
	}
	for id, value := range want {
		var got string
		if err := database.Pool.QueryRow(ctx, `SELECT content_sensitive_at_send FROM mail_tasks WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("%s: content_sensitive_at_send %q, want %q", id, got, value)
		}
	}

	// The copy is read first: a frozen send stays sensitive once its template
	// lets go of the lock and says false.
	if _, err := database.Pool.Exec(ctx,
		`UPDATE templates SET key = 'club.duyuru' WHERE id = '10000000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}
	var sensitive bool
	if err := database.Pool.QueryRow(ctx,
		`SELECT mail_task_content_sensitive('40000000-0000-4000-8000-000000000001')`).Scan(&sensitive); err != nil {
		t.Fatal(err)
	}
	if !sensitive {
		t.Error("a frozen send read as shown once its template's key changed")
	}
}
