package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The Required variable migration.
const requiredVariables = uint(20260923200000)

// Templates written before Required variables existed get two empty sets:
// nothing is required of them until the Template seed writes a contract or an
// operator marks a variable, so no body that is sent today becomes unsavable.
// Down takes the sets away and leaves every row as it was.
func TestRequiredVariableMigrationStartsEveryTemplateWithEmptySets(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(templateVersions); err != nil {
		t.Fatalf("migrate to %d: %v", templateVersions, err)
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

	if err := runner.Migrate(requiredVariables); err != nil {
		t.Fatalf("up: %v", err)
	}
	var withSets, total int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE contract_required_variables = '[]' AND operator_required_variables = '{}'), count(*)
		FROM templates`).Scan(&withSets, &total); err != nil {
		t.Fatal(err)
	}
	if total != len(existingTemplates) || withSets != total {
		t.Fatalf("templates with two empty sets = %d of %d, want all %d", withSets, total, len(existingTemplates))
	}
	if rows := templateRowsWithout(t, database, "contract_required_variables", "operator_required_variables"); !equalRows(rows, rowsBefore) {
		t.Fatalf("the migration changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	// The two sets never share a name, and hold only names a body can reach;
	// a contract entry is a name with a text or null reason, each name once.
	for statement, violated := range map[string]string{
		`UPDATE templates SET contract_required_variables = '[{"name":"link"}]', operator_required_variables = '{link}'`: "templates_required_variables_disjoint",
		`UPDATE templates SET operator_required_variables = '{"not a name"}'`:                                            "templates_operator_required_variables_valid",
		`UPDATE templates SET operator_required_variables = ARRAY[NULL]::text[]`:                                         "templates_operator_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '[{"name":"1link"}]'`:                                        "templates_contract_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '["link"]'`:                                                  "templates_contract_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '{"name":"link"}'`:                                           "templates_contract_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '[{"name":"link"},{"name":"link","reason":null}]'`:           "templates_contract_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '[{"name":"link","reason":3}]'`:                              "templates_contract_required_variables_valid",
		`UPDATE templates SET contract_required_variables = '[{"reason":"neden"}]'`:                                      "templates_contract_required_variables_valid",
	} {
		_, err := database.Pool.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != violated {
			t.Errorf("%s: err = %v, want a violation of %s", statement, err, violated)
		}
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE templates SET contract_required_variables = '[{"name":"link","reason":"Bağlantı"},{"name":"code","reason":null},{"name":"VerifyURL"}]', operator_required_variables = '{firstName}'`); err != nil {
		t.Fatalf("well-formed sets refused: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE templates SET contract_required_variables = '[]', operator_required_variables = '{}'`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(templateVersions); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, leftover := range []string{
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'templates' AND column_name LIKE '%required_variables')`,
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname IN ('required_variable_names_valid', 'contract_variable_names', 'contract_variables_valid'))`,
	} {
		var exists bool
		if err := database.Pool.QueryRow(ctx, leftover).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("down left behind: %s", leftover)
		}
	}
	if rows := templateRows(t, database); !equalRows(rows, rowsBefore) {
		t.Fatalf("down changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	if err := runner.Migrate(requiredVariables); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// templateRowsWithout is templateRows leaving out columns a later migration
// added, so rows can be compared across it.
func templateRowsWithout(t *testing.T, database testpostgres.Database, columns ...string) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		SELECT name, (to_jsonb(t) - 'published_version_id' - $1::text[])::text FROM templates t`, columns)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	byName := map[string]string{}
	for rows.Next() {
		var name, row string
		if err := rows.Scan(&name, &row); err != nil {
			t.Fatal(err)
		}
		byName[name] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return byName
}
