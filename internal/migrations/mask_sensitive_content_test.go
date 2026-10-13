package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const maskSensitiveContentAtRest = uint(20261013140000)

// Up leaves only the kept sends that are sensitive due for the sweep: the
// club's history that is not sensitive is not read first (#63 Ek 2, Y2).
func TestMaskSensitiveContentMigrationLeavesOnlySensitiveSendsDue(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailTasksContentKept); err != nil {
		t.Fatalf("migrate to %d: %v", mailTasksContentKept, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('10000000-0000-4000-8000-000000000001', 'core.welcome', 'Hoş geldin', 'K', '<p>x</p>', 'x', '', false),
		       ('10000000-0000-4000-8000-000000000002', 'forms.form-copy', 'Form', 'K', '<p>x</p>', 'x', '', NULL),
		       ('10000000-0000-4000-8000-000000000003', 'keycloak.reset-password', 'Parola', 'K', '<p>x</p>', 'x', '', true);
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, content_cleared_at) VALUES
		    ('40000000-0000-4000-8000-000000000001', 's', '10000000-0000-4000-8000-000000000001', NULL, '{}', NULL),
		    ('40000000-0000-4000-8000-000000000002', 's', '10000000-0000-4000-8000-000000000002', NULL, '{}', NULL),
		    ('40000000-0000-4000-8000-000000000003', 's', '10000000-0000-4000-8000-000000000002', gen_random_uuid(), '{}', NULL),
		    ('40000000-0000-4000-8000-000000000004', 's', '10000000-0000-4000-8000-000000000003', NULL, '{}', NULL),
		    ('40000000-0000-4000-8000-000000000005', 's', '10000000-0000-4000-8000-000000000003', NULL, '{}', now());`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(maskSensitiveContentAtRest); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := map[string]bool{
		"40000000-0000-4000-8000-000000000001": false, // shown single
		"40000000-0000-4000-8000-000000000002": true,  // single left to its kind: sensitive
		"40000000-0000-4000-8000-000000000003": false, // the same template to a list
		"40000000-0000-4000-8000-000000000004": true,  // locked key
		"40000000-0000-4000-8000-000000000005": false, // emptied
	}
	for id, due := range want {
		var got bool
		if err := database.Pool.QueryRow(ctx, `SELECT content_mask_due FROM mail_tasks WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != due {
			t.Errorf("%s: content_mask_due %v, want %v", id, got, due)
		}
	}
}
