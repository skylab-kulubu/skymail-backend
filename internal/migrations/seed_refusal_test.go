package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps refused Template seeds on their template.
const seedRefusals = uint(20260923220000)

// No template has a refused seed when the migration runs. A refusal is kept
// whole — when, which rules, the content's hash — with rules the seed knows
// and a SHA-256 in hex. Down takes it away and leaves every row as it was.
func TestSeedRefusalMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(requiredVariables); err != nil {
		t.Fatalf("migrate to %d: %v", requiredVariables, err)
	}
	for _, existing := range existingTemplates {
		if _, err := database.Pool.Exec(ctx, `
			INSERT INTO templates (name, key, system, subject, html_content, plain_text_content, react_email_content)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, existing.name, existing.key, existing.system, existing.subject, existing.html, existing.plainText, existing.react); err != nil {
			t.Fatalf("insert %s: %v", existing.name, err)
		}
	}
	rowsBefore := templateRows(t, database)

	if err := runner.Migrate(seedRefusals); err != nil {
		t.Fatalf("up: %v", err)
	}
	var refused int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FROM templates
		WHERE seed_refused_at IS NOT NULL OR seed_refused_rules IS NOT NULL OR seed_refused_payload_sha256 IS NOT NULL`).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	if refused != 0 {
		t.Fatalf("templates with a refused seed after the migration = %d, want none", refused)
	}
	if rows := templateRowsWithout(t, database, "seed_refused_at", "seed_refused_rules", "seed_refused_payload_sha256"); !equalRows(rows, rowsBefore) {
		t.Fatalf("the migration changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	const hash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for statement, violated := range map[string]string{
		`UPDATE templates SET seed_refused_at = NOW()`:                                                                                  "templates_seed_refusal_whole",
		`UPDATE templates SET seed_refused_at = NOW(), seed_refused_rules = '{newer_operator_version}'`:                                 "templates_seed_refusal_whole",
		`UPDATE templates SET seed_refused_at = NOW(), seed_refused_rules = '{}', seed_refused_payload_sha256 = '` + hash + `'`:         "templates_seed_refused_rules_known",
		`UPDATE templates SET seed_refused_at = NOW(), seed_refused_rules = '{operator}', seed_refused_payload_sha256 = '` + hash + `'`: "templates_seed_refused_rules_known",
		`UPDATE templates SET seed_refused_at = NOW(), seed_refused_rules = '{operator_subject}', seed_refused_payload_sha256 = 'abc'`:  "templates_seed_refused_payload_sha256_hex",
	} {
		_, err := database.Pool.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != violated {
			t.Errorf("%s: err = %v, want a violation of %s", statement, err, violated)
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		UPDATE templates SET seed_refused_at = NOW(), seed_refused_rules = '{published_by_operator,newer_operator_version,operator_subject}',
		                     seed_refused_payload_sha256 = '`+hash+`'`); err != nil {
		t.Fatalf("a whole refusal refused: %v", err)
	}

	if err := runner.Migrate(requiredVariables); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left bool
	if err := database.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'templates' AND column_name LIKE 'seed_refused%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left {
		t.Error("down left seed_refused columns behind")
	}
	if rows := templateRows(t, database); !equalRows(rows, rowsBefore) {
		t.Fatalf("down changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
