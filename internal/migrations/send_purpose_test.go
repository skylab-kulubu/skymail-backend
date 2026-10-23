package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps a send's purpose and category (ADR 0072 decision
// 2; hub ticket abonelik-cikisi 03).
const sendPurpose = uint(20261023100000)

// The old image's writes, as its sqlc spelled them: it names its columns, so
// it never writes the new ones.
const (
	oldImageInsertTask = `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested)
		VALUES ('sub', $1, '{}', 'unset', false) RETURNING id`
	oldImageCreateApproval = `
		INSERT INTO mail_approvals (submitter_sub, template_id, template_version_id, mail_list_id, body_variables, created_at,
		                            submitted_at, deadline_at, updated_at, tracking_requested)
		VALUES ('sub', $1, $2, gen_random_uuid(), '{}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW(), false) RETURNING id`
	oldImageResubmit = `
		UPDATE mail_approvals SET state = 'pending', body_variables = '{"a":1}', tracking_requested = false,
		       submitted_at = NOW(), deadline_at = NOW() + INTERVAL '7 days', updated_at = NOW()
		WHERE id = $1`
)

// Up: four NULL columns on mail_tasks and mail_approvals, the rows already
// there and the old image's new rows NULL in all four; a new row holds a
// purpose, core's category and the statements of its purpose only. Down drops
// them, and up again comes back.
func TestSendsKeepTheirPurposeAndCategory(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(unsubscribes); err != nil {
		t.Fatalf("migrate to %d: %v", unsubscribes, err)
	}
	var templateID, versionID string
	if err := database.Pool.QueryRow(ctx, `
		WITH template AS (
			INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
			VALUES ('Serbest Gönderim', '{{.Subject}}', '<p>x</p>', 'x', '') RETURNING id),
		     version AS (
			INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content,
			                               plain_text_content, author_kind, published_at)
			SELECT id, 1, 'Serbest Gönderim', '{{.Subject}}', '<p>x</p>', 'html', '<p>x</p>', 'x', 'template_seed', NOW()
			FROM template RETURNING id, template_id)
		SELECT template_id::text, id::text FROM version`).Scan(&templateID, &versionID); err != nil {
		t.Fatal(err)
	}
	var beforeTask, beforeApproval string
	if err := database.Pool.QueryRow(ctx, oldImageInsertTask, templateID).Scan(&beforeTask); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, oldImageCreateApproval, templateID, versionID).Scan(&beforeApproval); err != nil {
		t.Fatal(err)
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name IN ('mail_tasks', 'mail_approvals')
			  AND column_name IN ('purpose', 'category', 'transactional_statement', 'commercial_content_absent')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := runner.Migrate(sendPurpose); err != nil {
		t.Fatalf("up: %v", err)
	}
	if n := columns(); n != 8 {
		t.Fatalf("after up: %d columns, want 8", n)
	}

	// The old image keeps working on the new schema.
	var oldTask, oldApproval string
	if err := database.Pool.QueryRow(ctx, oldImageInsertTask, templateID).Scan(&oldTask); err != nil {
		t.Fatalf("the old image's task insert: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, oldImageCreateApproval, templateID, versionID).Scan(&oldApproval); err != nil {
		t.Fatalf("the old image's request insert: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, oldImageResubmit, oldApproval); err != nil {
		t.Fatalf("the old image's resubmit: %v", err)
	}
	var unset int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM mail_tasks WHERE purpose IS NULL AND category IS NULL AND transactional_statement IS NULL AND commercial_content_absent IS NULL AND id IN ($1, $2))
		     + (SELECT count(*) FROM mail_approvals WHERE purpose IS NULL AND category IS NULL AND transactional_statement IS NULL AND commercial_content_absent IS NULL AND id IN ($3, $4))`,
		beforeTask, oldTask, beforeApproval, oldApproval).Scan(&unset); err != nil || unset != 4 {
		t.Fatalf("rows without a purpose = %d (%v), want the 4 written before and by the old image", unset, err)
	}

	write := func(table, purpose, category, transactional, commercial string) error {
		if table == "mail_tasks" {
			_, err := database.Pool.Exec(ctx, `
				INSERT INTO mail_tasks (sent_by, template_id, body_variables, purpose, category, transactional_statement, commercial_content_absent)
				VALUES ('sub', $1, '{}', NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, '')::boolean, NULLIF($5, '')::boolean)`,
				templateID, purpose, category, transactional, commercial)
			return err
		}
		_, err := database.Pool.Exec(ctx, `
			UPDATE mail_approvals SET purpose = NULLIF($2, ''), category = NULLIF($3, ''),
			       transactional_statement = NULLIF($4, '')::boolean, commercial_content_absent = NULLIF($5, '')::boolean
			WHERE id = $1`, oldApproval, purpose, category, transactional, commercial)
		return err
	}
	for _, table := range []string{"mail_tasks", "mail_approvals"} {
		for _, ok := range [][4]string{
			{"duyuru", "club:duyuru", "", ""},
			{"duyuru", "club:haber", "", "true"},
			{"duyuru", "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:haber", "", "false"},
			{"islem", "", "true", ""},
			{"islem", "", "", ""},
			{"", "", "", ""},
		} {
			if err := write(table, ok[0], ok[1], ok[2], ok[3]); err != nil {
				t.Errorf("%s %v: %v", table, ok, err)
			}
		}
		for _, bad := range []struct {
			fields     [4]string
			constraint string
		}{
			{[4]string{"ticari", "", "", ""}, "_purpose_check"},
			{[4]string{"duyuru", "club:bulten", "", ""}, "_category_check"},
			{[4]string{"duyuru", "team:g-airlab:duyuru", "", ""}, "_category_check"},
			{[4]string{"duyuru", "team:6F1D2C3B-0A9E-4F8D-8C7B-6A5E4D3C2B1A:haber", "", ""}, "_category_check"},
			{[4]string{"duyuru", "", "", ""}, "_purpose_fields_check"},
			{[4]string{"duyuru", "club:duyuru", "true", ""}, "_purpose_fields_check"},
			{[4]string{"islem", "club:duyuru", "true", ""}, "_purpose_fields_check"},
			{[4]string{"islem", "", "true", "true"}, "_purpose_fields_check"},
			{[4]string{"", "club:duyuru", "", ""}, "_purpose_fields_check"},
			{[4]string{"", "", "true", ""}, "_purpose_fields_check"},
		} {
			if err := write(table, bad.fields[0], bad.fields[1], bad.fields[2], bad.fields[3]); err == nil || !strings.Contains(err.Error(), table+bad.constraint) {
				t.Errorf("%s %v: %v, want %s%s", table, bad.fields, err, table, bad.constraint)
			}
		}
	}

	// A send to a list is never islem.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, purpose, transactional_statement)
		VALUES ('sub', $1, gen_random_uuid(), '{}', 'islem', true)`, templateID); err == nil || !strings.Contains(err.Error(), "mail_tasks_purpose_audience_check") {
		t.Errorf("islem to a list: %v, want mail_tasks_purpose_audience_check", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, purpose, category)
		VALUES ('sub', $1, gen_random_uuid(), '{}', 'duyuru', 'club:duyuru')`, templateID); err != nil {
		t.Errorf("duyuru to a list: %v", err)
	}

	if err := runner.Migrate(unsubscribes); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := columns(); n != 0 {
		t.Errorf("after down: %d columns", n)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := columns(); n != 8 {
		t.Errorf("after up again: %d columns", n)
	}
}
