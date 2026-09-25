package migrations_test

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that makes every function in the schema find what it calls
// whatever the caller's search_path.
const functionSearchPath = uint(20260925120000)

// A dump of SkyMail restores with pg_restore as it is. pg_restore empties
// search_path while it loads the rows, and the templates' CHECKs call
// contract_variables_valid(), which calls two other functions; the restored
// database's functions then work with no search_path at all.
func TestADumpRestoresWithPlainPgRestore(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up: %v", err)
	}

	// A template with a contract and an operator's variable, its version, a
	// send with its queue row and a request to one person: rows every CHECK
	// and function reads.
	if _, err := database.Pool.Exec(ctx, `
		WITH template AS (
			INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content,
			                       contract_required_variables, operator_required_variables)
			VALUES ('Serbest Gönderim', '{{.Subject}}', '<p>{{.BodyHtml}}</p>', '{{.BodyHtml}}', '',
			        '[{"name":"Subject","reason":"Konu gönderimde yazılır."}]', '{BodyHtml}')
			RETURNING id),
		     version AS (
			INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content,
			                               plain_text_content, author_kind, published_at)
			SELECT id, 1, 'Serbest Gönderim', '{{.Subject}}', '<p>{{.BodyHtml}}</p>', 'html', '<p>{{.BodyHtml}}</p>',
			       '{{.BodyHtml}}', 'template_seed', NOW()
			FROM template
			RETURNING id, template_id),
		     approval AS (
			INSERT INTO mail_approvals (submitter_sub, template_id, template_version_id, body_variables,
			                            created_at, submitted_at, deadline_at, updated_at)
			SELECT 'sub', template_id, id, '{}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW()
			FROM version
			RETURNING id),
		     person AS (
			INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
			SELECT id, 1, 'ayse@example.com', 'Ayşe Kaya' FROM approval),
		     task AS (
			INSERT INTO mail_tasks (sent_by, template_id, body_variables)
			SELECT 'sub', id, '{}' FROM template
			RETURNING id)
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status)
		SELECT id, 'Ayşe Kaya', 'ayse@example.com', 'Konu', 'Gövde', 'sent' FROM task`); err != nil {
		t.Fatal(err)
	}

	docker := func(args ...string) {
		t.Helper()
		out, err := exec.Command("docker", append([]string{"exec", database.Container}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	docker("pg_dump", "-U", "postgres", "-d", "skymailtest", "-Fc", "-f", "/tmp/skymail.dump")
	docker("createdb", "-U", "postgres", "restored")
	docker("pg_restore", "-U", "postgres", "-d", "restored", "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction",
		"/tmp/skymail.dump")

	restored, err := pgx.Connect(ctx, strings.Replace(database.URL, "/skymailtest?", "/restored?", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(ctx) })

	if _, err := restored.Exec(ctx, `SET search_path TO ''`); err != nil {
		t.Fatal(err)
	}
	var templates, people int
	var status string
	if err := restored.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM public.templates),
		       (SELECT count(*) FROM public.mail_approval_recipients),
		       (SELECT public.mail_task_status(id) FROM public.mail_tasks)`).Scan(&templates, &people, &status); err != nil {
		t.Fatalf("reading the restored rows with no search_path: %v", err)
	}
	if templates != 1 || people != 1 || status != "sent" {
		t.Fatalf("restored %d templates, %d people, a send %q; want 1, 1, sent", templates, people, status)
	}
	// The audience check still finds its tables: a request to no one is
	// refused for going to no one, not for a table it cannot see.
	_, err = restored.Exec(ctx, `
		INSERT INTO public.mail_approvals (submitter_sub, template_id, template_version_id, body_variables,
		                                   created_at, submitted_at, deadline_at, updated_at)
		SELECT 'sub', template_id, id, '{}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW() FROM public.template_versions`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "mail_approvals_one_audience" {
		t.Fatalf("a request to no one with no search_path: %v, want 23514 on mail_approvals_one_audience", err)
	}
}

// Every function in the schema pins its search_path, so none depends on the
// caller's: a new one that does not is named here. Down takes the pins away
// and leaves the functions as they were.
func TestEveryFunctionPinsItsSearchPath(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(functionSearchPath); err != nil {
		t.Fatalf("up: %v", err)
	}

	unpinned := func() []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			SELECT p.oid::regprocedure::text
			FROM pg_proc p
			         JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public'
			  AND NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) AS setting WHERE setting LIKE 'search_path=%')
			ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if got := unpinned(); len(got) != 0 {
		t.Fatalf("functions with no search_path of their own: %v; pin each with ALTER FUNCTION … SET search_path = pg_catalog, public", got)
	}

	if err := runner.Migrate(approvalRecipients); err != nil {
		t.Fatalf("down: %v", err)
	}
	all := []string{
		"check_mail_approval()",
		"contract_variable_names(jsonb)",
		"contract_variables_valid(jsonb)",
		"mail_task_status(uuid)",
		"required_variable_names_valid(text[])",
		"template_jsx_source(text)",
	}
	if got := unpinned(); !slices.Equal(got, all) {
		t.Fatalf("after down, functions with no search_path of their own = %v, want every one: %v", got, all)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
