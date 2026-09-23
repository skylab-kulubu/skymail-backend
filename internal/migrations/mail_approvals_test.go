package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps Mail onayı requests and their history.
const mailApprovals = uint(20260923233000)

// A request holds one audience, a template version of its own template, a
// deadline after its submission and a send only once approved; its history
// names who did each thing but the expiry, and a rejection always carries its
// reason. Down takes both tables away and leaves the sends and templates.
func TestMailApprovalMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailApprovals); err != nil {
		t.Fatalf("up: %v", err)
	}

	var templateID, versionID, otherVersionID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Serbest Gönderim', '{{.Subject}}', '<p>{{.Body}}</p>', '{{.Body}}', '')
		RETURNING id`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content, plain_text_content,
		                               author_kind, published_at)
		VALUES ($1, 1, 'Serbest Gönderim', '{{.Subject}}', '<p>{{.Body}}</p>', 'html', '<p>{{.Body}}</p>', '{{.Body}}', 'operator', NOW())
		RETURNING id`, templateID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `
		WITH other AS (INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		               VALUES ('Başka', 'x', 'x', 'x', '') RETURNING id)
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content, plain_text_content, author_kind)
		SELECT id, 1, 'Başka', 'x', 'x', 'html', 'x', 'x', 'operator' FROM other
		RETURNING id`).Scan(&otherVersionID); err != nil {
		t.Fatal(err)
	}

	insert := func(version string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_approvals (submitter_sub, template_id, template_version_id, recipient_email, body_variables,
			                            created_at, submitted_at, deadline_at, updated_at)
			VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', $1, $2, 'uye@yildizskylab.com', '{"Body":"x"}',
			        NOW(), NOW(), NOW() + INTERVAL '7 days', NOW())`, templateID, version)
		return err
	}
	if err := insert(versionID); err != nil {
		t.Fatalf("a single-recipient request refused: %v", err)
	}

	wantViolation := func(what string, err error, code, constraint string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
			t.Errorf("%s: err = %v, want %s on %s", what, err, code, constraint)
		}
	}
	wantViolation("a version of another template", insert(otherVersionID), "23503", "mail_approvals_template_version")

	for statement, constraint := range map[string]string{
		`UPDATE mail_approvals SET mail_list_id = gen_random_uuid()`:                                                     "mail_approvals_one_audience",
		`UPDATE mail_approvals SET recipient_email = NULL`:                                                               "mail_approvals_one_audience",
		`UPDATE mail_approvals SET body_variables = '[]'`:                                                                "mail_approvals_variables_object",
		`UPDATE mail_approvals SET deadline_at = submitted_at`:                                                           "mail_approvals_deadline_after_submission",
		`UPDATE mail_approvals SET submitted_at = created_at - INTERVAL '1 second'`:                                      "mail_approvals_submitted_after_created",
		`UPDATE mail_approvals SET mail_list_id = gen_random_uuid(), recipient_email = NULL, recipient_full_name = 'Ad'`: "mail_approvals_recipient_name",
	} {
		_, err := database.Pool.Exec(ctx, statement)
		wantViolation(statement, err, "23514", constraint)
	}

	var taskID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables) VALUES ('x', $1, '{}') RETURNING id`, templateID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	_, err := database.Pool.Exec(ctx, `UPDATE mail_approvals SET task_id = $1`, taskID)
	wantViolation("a send on a pending request", err, "23514", "mail_approvals_task_only_when_approved")
	if _, err := database.Pool.Exec(ctx, `UPDATE mail_approvals SET task_id = $1, state = 'approved'`, taskID); err != nil {
		t.Fatalf("an approved request's send refused: %v", err)
	}

	var approvalID string
	if err := database.Pool.QueryRow(ctx, `SELECT id FROM mail_approvals`).Scan(&approvalID); err != nil {
		t.Fatal(err)
	}
	event := func(seq int, kind, actor, note, changes string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_approval_events (approval_id, seq, kind, actor_sub, note, changes, created_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, '')::jsonb, NOW())`,
			approvalID, seq, kind, actor, note, changes)
		return err
	}
	if err := event(1, "submitted", "sub", "", ""); err != nil {
		t.Fatalf("a submission refused: %v", err)
	}
	if err := event(2, "edited", "sub", "", `[{"field":"variable","name":"Body","before":"x","after":"y"}]`); err != nil {
		t.Fatalf("an edit refused: %v", err)
	}
	if err := event(3, "expired", "", "", ""); err != nil {
		t.Fatalf("an expiry refused: %v", err)
	}
	wantViolation("a second event 1", event(1, "approved", "sub", "", ""), "23505", "mail_approval_events_approval_seq")
	wantViolation("an expiry with an actor", event(4, "expired", "sub", "", ""), "23514", "mail_approval_events_actor")
	wantViolation("an approval with no actor", event(4, "approved", "", "", ""), "23514", "mail_approval_events_actor")
	wantViolation("a rejection with no reason", event(4, "rejected", "sub", "", ""), "23514", "mail_approval_events_rejection_reason")
	wantViolation("a rejection with a blank reason", event(4, "rejected", "sub", "  ", ""), "23514", "mail_approval_events_rejection_reason")
	wantViolation("changes on an approval", event(4, "approved", "sub", "", `[]`), "23514", "mail_approval_events_changes_of_edits")
	wantViolation("changes that are not a list", event(4, "edited", "sub", "", `{}`), "23514", "mail_approval_events_changes_array")
	if err := event(4, "rejected", "sub", "Tarih yanlış.", ""); err != nil {
		t.Fatalf("a rejection with a reason refused: %v", err)
	}

	if err := runner.Migrate(versionNames); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'mail_approval%')
		     + (SELECT count(*) FROM pg_type WHERE typname LIKE 'mail_approval%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("tables and types left after down = %d, want none", left)
	}
	var tasks, templates int
	if err := database.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mail_tasks), (SELECT count(*) FROM templates)`).Scan(&tasks, &templates); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || templates != 2 {
		t.Fatalf("after down: %d sends, %d templates, want 1 and 2", tasks, templates)
	}
}
