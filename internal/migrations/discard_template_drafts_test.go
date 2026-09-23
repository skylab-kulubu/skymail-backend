package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration before drafts could be discarded.
const beforeDiscardedDrafts = uint(20260923120000)

// Discarding is for drafts: a version can be discarded while it is
// unpublished, never once published. Down makes discarded drafts plain drafts
// again and leaves every version otherwise as it was; up again restores the
// column empty.
func TestDiscardedDraftMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatal(err)
	}

	var templateID, published, draft string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Bülten', 'Bülten', '<p>Bülten</p>', 'Bülten', '')
		RETURNING id`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	insert := func(seq int, publishedAt string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, `
			INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content, plain_text_content,
			                               author_kind, published_at)
			VALUES ($1, $2, 'Bülten', 'Bülten', '<p>Bülten</p>', 'html', '<p>Bülten</p>', 'Bülten', 'operator', `+publishedAt+`)
			RETURNING id`, templateID, seq).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	published, draft = insert(1, "NOW()"), insert(2, "NULL")

	if _, err := database.Pool.Exec(ctx, `UPDATE template_versions SET discarded_at = NOW() WHERE id = $1`, published); err == nil {
		t.Fatal("a published version was discarded")
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE template_versions SET discarded_at = NOW() WHERE id = $1`, draft); err != nil {
		t.Fatalf("discarding a draft: %v", err)
	}
	var summarised bool
	if err := database.Pool.QueryRow(ctx, `SELECT discarded_at IS NOT NULL FROM template_version_summaries WHERE id = $1`, draft).Scan(&summarised); err != nil || !summarised {
		t.Fatalf("summary of the discarded draft = %v %v, want it discarded", summarised, err)
	}

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeDiscardedDrafts); err != nil {
		t.Fatalf("down: %v", err)
	}
	var column, versions int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns WHERE table_name IN ('template_versions', 'template_version_summaries') AND column_name = 'discarded_at'),
		       (SELECT count(*) FROM template_version_summaries WHERE template_id = $1)`, templateID).Scan(&column, &versions); err != nil {
		t.Fatal(err)
	}
	if column != 0 || versions != 2 {
		t.Fatalf("after down: discarded_at in %d relations, %d versions summarised; want none and both", column, versions)
	}

	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	var discarded int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM template_versions WHERE discarded_at IS NOT NULL`).Scan(&discarded); err != nil || discarded != 0 {
		t.Fatalf("discarded after down and up = %d %v, want none", discarded, err)
	}
}
