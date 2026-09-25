package migrations_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that lets a Mail onayı request go to several people, each
// with a send of their own.
const approvalRecipients = uint(20260925100000)

// Every request that went to one person goes to that person from its own
// table, and an approved one keeps its send. A request goes to a list or to 1
// to 100 people, each address once, never both and never neither; only an
// approved request has sends. Down puts one person and one send back on the
// request, and refuses — changing nothing — while a request goes to several.
func TestMailApprovalRecipientsMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	pool := database.Pool

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailApprovals); err != nil {
		t.Fatalf("migrate to %d: %v", mailApprovals, err)
	}

	var templateID, versionID, taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO templates (name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Serbest Gönderim', '{{.Subject}}', '<p>{{.Body}}</p>', '{{.Body}}', '')
		RETURNING id`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content, plain_text_content,
		                               author_kind, published_at)
		VALUES ($1, 1, 'Serbest Gönderim', '{{.Subject}}', '<p>{{.Body}}</p>', 'html', '<p>{{.Body}}</p>', '{{.Body}}', 'operator', NOW())
		RETURNING id`, templateID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables) VALUES ('x', $1, '{}') RETURNING id`, templateID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	request := func(state, list, email, name, task string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO mail_approvals (submitter_sub, state, template_id, template_version_id, mail_list_id, recipient_email,
			                            recipient_full_name, body_variables, created_at, submitted_at, deadline_at, updated_at, task_id)
			VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', $1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, ''), NULLIF($6, ''),
			        '{"Body":"x"}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW(), NULLIF($7, '')::uuid)
			RETURNING id`, state, templateID, versionID, list, email, name, task).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending := request("pending", "", "ayse@example.com", "Ayşe Kaya", "")
	approved := request("approved", "", "mehmet@example.com", "Mehmet Demir", taskID)
	nameless := request("rejected", "", "adsiz@example.com", "", "")
	toList := request("pending", "3f0c1a52-0000-4000-8000-000000000001", "", "", "")

	if err := runner.Migrate(approvalRecipients); err != nil {
		t.Fatalf("up: %v", err)
	}

	type person struct{ approval, email, name string }
	people := func() []person {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT approval_id::text, position, email, full_name FROM mail_approval_recipients ORDER BY email, position`)
		if err != nil {
			t.Fatal(err)
		}
		var out []person
		for rows.Next() {
			var p person
			var position int
			if err := rows.Scan(&p.approval, &position, &p.email, &p.name); err != nil {
				t.Fatal(err)
			}
			if position != 1 {
				t.Errorf("%s is at position %d, want 1", p.email, position)
			}
			out = append(out, p)
		}
		return out
	}
	want := []person{{nameless, "adsiz@example.com", ""}, {pending, "ayse@example.com", "Ayşe Kaya"}, {approved, "mehmet@example.com", "Mehmet Demir"}}
	if got := people(); !slices.Equal(got, want) {
		t.Fatalf("people after up = %+v, want %+v", got, want)
	}
	rows, err := pool.Query(ctx, `SELECT approval_id::text || ' ' || position || ' ' || task_id::text FROM mail_approval_tasks`)
	if err != nil {
		t.Fatal(err)
	}
	sends, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(sends) != 1 || sends[0] != approved+" 1 "+taskID {
		t.Fatalf("sends after up = %v, want the approved request's one send", sends)
	}
	var oldColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'mail_approvals' AND column_name IN ('recipient_email', 'recipient_full_name', 'task_id')`).Scan(&oldColumns); err != nil {
		t.Fatal(err)
	}
	if oldColumns != 0 {
		t.Fatalf("%d of the one-person columns are left on mail_approvals", oldColumns)
	}

	// Each statement below is its own transaction; the audience is checked
	// when it commits, so a request and its people may be written in turn.
	inTx := func(statements ...string) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	wantViolation := func(what string, err error, code, constraint string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
			t.Errorf("%s: err = %v, want %s on %s", what, err, code, constraint)
		}
	}
	addPerson := func(approval string, position int, email string) string {
		return `INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
		        VALUES ('` + approval + `', ` + strconv.Itoa(position) + `, '` + email + `', '')`
	}
	newRequest := func(list string) string {
		return `INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, mail_list_id, body_variables,
		                                    created_at, submitted_at, deadline_at, updated_at)
		        VALUES ('7a1e0000-0000-4000-8000-000000000001', 'sub', '` + templateID + `', '` + versionID + `', ` + list + `,
		                '{}', NOW(), NOW(), NOW() + INTERVAL '7 days', NOW())`
	}

	wantViolation("a list request with a person too", inTx(addPerson(toList, 1, "uye@example.com")), "23514", "mail_approvals_one_audience")
	wantViolation("a request's only person removed", inTx(`DELETE FROM mail_approval_recipients WHERE approval_id = '`+pending+`'`),
		"23514", "mail_approvals_one_audience")
	wantViolation("a person's request turned to a list",
		inTx(`UPDATE mail_approvals SET mail_list_id = gen_random_uuid() WHERE id = '`+pending+`'`), "23514", "mail_approvals_one_audience")
	wantViolation("a request to no one", inTx(newRequest("NULL")), "23514", "mail_approvals_one_audience")
	if err := inTx(newRequest("NULL"), addPerson("7a1e0000-0000-4000-8000-000000000001", 1, "yeni@example.com")); err != nil {
		t.Fatalf("a request written with its person refused: %v", err)
	}
	wantViolation("an address twice, in another case", inTx(addPerson(pending, 2, "AYSE@example.com")), "23505", "mail_approval_recipients_email_once")
	wantViolation("a 101st person", inTx(addPerson(pending, 101, "yuzbir@example.com")), "23514", "mail_approval_recipients_position")
	wantViolation("a blank address", inTx(addPerson(pending, 2, " ")), "23514", "mail_approval_recipients_email")
	wantViolation("a send on a pending request", inTx(`
		WITH send AS (INSERT INTO mail_tasks (sent_by, template_id, body_variables) VALUES ('x', '`+templateID+`', '{}') RETURNING id)
		INSERT INTO mail_approval_tasks (approval_id, position, task_id) SELECT '`+pending+`', 1, id FROM send`),
		"23514", "mail_approvals_sends_when_approved")
	if err := inTx(addPerson(pending, 2, "mehmet@example.com")); err != nil {
		t.Fatalf("a second person refused: %v", err)
	}

	// Down cannot put two people on one request: it refuses, and leaves
	// everything as it was, marked dirty for an operator to see.
	err = runner.Migrate(mailApprovals)
	if err == nil || !strings.Contains(err.Error(), "more than one person") {
		t.Fatalf("down with a request to two people = %v, want a refusal", err)
	}
	var stillThere int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mail_approval_recipients`).Scan(&stillThere); err != nil {
		t.Fatalf("after the refused down: %v", err)
	}
	if stillThere != 5 {
		t.Fatalf("after the refused down: %d people, want all 5 still there", stillThere)
	}
	if err := runner.Force(int(approvalRecipients)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM mail_approval_recipients WHERE position = 2;
		DELETE FROM mail_approval_recipients WHERE approval_id = '7a1e0000-0000-4000-8000-000000000001';
		DELETE FROM mail_approvals WHERE id = '7a1e0000-0000-4000-8000-000000000001'`); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailApprovals); err != nil {
		t.Fatalf("down: %v", err)
	}
	type row struct{ id, email, name, task string }
	var got []row
	rows, err = pool.Query(ctx, `
		SELECT id::text, COALESCE(recipient_email, ''), COALESCE(recipient_full_name, ''), COALESCE(task_id::text, '')
		FROM mail_approvals ORDER BY COALESCE(recipient_email, '')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.email, &r.name, &r.task); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	wantRows := []row{{toList, "", "", ""}, {nameless, "adsiz@example.com", "", ""}, {pending, "ayse@example.com", "Ayşe Kaya", ""},
		{approved, "mehmet@example.com", "Mehmet Demir", taskID}}
	if !slices.Equal(got, wantRows) {
		t.Fatalf("after down: %+v, want %+v", got, wantRows)
	}
	_, err = pool.Exec(ctx, `UPDATE mail_approvals SET recipient_email = NULL WHERE id = $1`, pending)
	wantViolation("a request to no one after down", err, "23514", "mail_approvals_one_audience")
	var left int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.tables WHERE table_name IN ('mail_approval_recipients', 'mail_approval_tasks'))
		     + (SELECT count(*) FROM pg_proc WHERE proname = 'check_mail_approval')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d of the tables and the check are left after down", left)
	}

	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := people(); !slices.Equal(got, want) {
		t.Fatalf("people after up again = %+v, want %+v", got, want)
	}
}
