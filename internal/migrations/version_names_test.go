package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that makes a template's name part of its versions.
const versionNames = uint(20260923230000)

// Every version that exists when names start being kept takes its template's
// name, the only one known, and the summaries serve it. Down forgets the
// names and leaves templates and versions otherwise as they were.
func TestVersionNameMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(seedRefusals); err != nil {
		t.Fatalf("migrate to %d: %v", seedRefusals, err)
	}
	var templateID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, key, system, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Hoş Geldin', 'core.welcome', true, 'Hoş geldin', '<p>Hoş geldin</p>', 'Hoş geldin', '')
		RETURNING id`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	for seq, published := range []string{"NOW()", "NULL"} {
		if _, err := database.Pool.Exec(ctx, `
			INSERT INTO template_versions (template_id, seq, subject, html_source, main_mode, html_content, plain_text_content,
			                               author_kind, published_at)
			VALUES ($1, $2, 'Hoş geldin', '<p>Hoş geldin</p>', 'html', '<p>Hoş geldin</p>', 'Hoş geldin', 'operator', `+published+`)`,
			templateID, seq+1); err != nil {
			t.Fatal(err)
		}
	}
	rowsBefore := templateRows(t, database)

	if err := runner.Migrate(versionNames); err != nil {
		t.Fatalf("up: %v", err)
	}
	var named, summarised int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM template_versions WHERE name = 'Hoş Geldin'),
		       (SELECT count(*) FROM template_version_summaries WHERE name = 'Hoş Geldin')`).Scan(&named, &summarised); err != nil {
		t.Fatal(err)
	}
	if named != 2 || summarised != 2 {
		t.Fatalf("versions named after their template = %d, summarised so = %d; want both 2", named, summarised)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO template_versions (template_id, seq, subject, html_source, main_mode, html_content, plain_text_content, author_kind)
		VALUES ($1, 3, 'x', 'x', 'html', 'x', 'x', 'operator')`, templateID); err == nil {
		t.Fatal("a version without a name was written")
	}
	if rows := templateRows(t, database); !equalRows(rows, rowsBefore) {
		t.Fatalf("the migration changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	if err := runner.Migrate(seedRefusals); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, versions int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE table_name IN ('template_versions', 'template_version_summaries') AND column_name = 'name'),
		       (SELECT count(*) FROM template_version_summaries WHERE template_id = $1)`, templateID).Scan(&columns, &versions); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || versions != 2 {
		t.Fatalf("after down: name in %d relations, %d versions summarised; want none and both", columns, versions)
	}

	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
