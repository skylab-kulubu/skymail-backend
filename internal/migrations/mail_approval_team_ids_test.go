package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const mailApprovalTeamIDs = uint(20261108120000)

// Team mail's Teams: NULL for every request before; a request's first Team
// is its group, never with a Forms source, 1..50 Teams, no NULL among them.
// Down drops the column and its check, and leaves the requests.
func TestMailApprovalTeamIDsMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(eraseAfterSend); err != nil {
		t.Fatalf("migrate to %d: %v", eraseAfterSend, err)
	}
	var template, version string
	if err := database.Pool.QueryRow(ctx, `
		WITH template AS (
			INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
			VALUES ('Serbest Gönderim', '{{.Subject}}', '<p>x</p>', 'x', '') RETURNING id)
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, published_at)
		SELECT id, 1, 'Serbest Gönderim', '{{.Subject}}', '<p>x</p>', 'html', '<p>x</p>', 'x', 'template_seed', NOW()
		FROM template RETURNING template_id, id`).Scan(&template, &version); err != nil {
		t.Fatal(err)
	}
	insert := func(list any, teams string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_approvals (submitter_sub, template_id, template_version_id, mail_list_id, body_variables,
			                            created_at, submitted_at, deadline_at, updated_at, team_ids)
			VALUES ('sub', $1, $2, $3::uuid, '{}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW(), $4::uuid[])`,
			template, version, list, teams)
		return err
	}
	const first, second = "10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002"
	if err := insert(first, "{"+first+"}"); err == nil {
		t.Fatal("team_ids was written before its migration")
	}
	if err := runner.Migrate(mailApprovalTeamIDs); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := insert(first, "{"+first+","+second+"}"); err != nil {
		t.Fatalf("a Team mail request: %v", err)
	}
	for name, teams := range map[string]string{
		"group not the first Team": "{" + second + "," + first + "}",
		"no Team":                  "{}",
		"a NULL Team":              "{" + first + ",NULL}",
	} {
		if err := insert(first, teams); err == nil {
			t.Errorf("%s was written", name)
		}
	}
	// No group at all: NULL = team_ids[1] is no answer, and a CHECK passes
	// on no answer, so the group is required in its own right.
	var refused *pgconn.PgError
	if err := insert(nil, "{"+first+"}"); !errors.As(err, &refused) || refused.ConstraintName != "mail_approvals_team_ids" {
		t.Errorf("Team mail without a group: %v, want mail_approvals_team_ids", err)
	}
	var withTeams int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_approvals WHERE team_ids IS NOT NULL`).Scan(&withTeams); err != nil || withTeams != 1 {
		t.Fatalf("requests with Teams = %d (%v)", withTeams, err)
	}

	if err := runner.Migrate(eraseAfterSend); err != nil {
		t.Fatalf("down: %v", err)
	}
	var column, check bool
	var requests int
	if err := database.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'mail_approvals' AND column_name = 'team_ids'),
		       EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'mail_approvals_team_ids'),
		       (SELECT count(*) FROM mail_approvals)`).Scan(&column, &check, &requests); err != nil {
		t.Fatal(err)
	}
	if column || check || requests != 1 {
		t.Fatalf("after down: column %v, check %v, %d requests", column, check, requests)
	}
}
