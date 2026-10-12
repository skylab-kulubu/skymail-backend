package mailer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// A template that renders for most recipients and fails for one: the initial
// of an empty name is out of range. A Keycloak user or a list recipient
// without a name is enough to hit it.

var (
	initialTemplate = uuid.MustParse("60000000-0000-4000-8000-000000000001")
	initialList     = uuid.MustParse("60000000-0000-4000-8000-000000000002")
)

// initialQueue is a migrated database with the initial template and a
// mailing list of Ayşe, a recipient without a name, and Mehmet.
func initialQueue(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ($1, 'Baş harf', 'Merhaba', '<p>{{slice .FullName 0 1}}.</p>', '{{.FullName}}', '');
		`, initialTemplate); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `INSERT INTO mailing_lists (id, name) VALUES ($1, 'Duyuru')`, initialList); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		WITH people AS (
			INSERT INTO recipients (full_name, email)
			VALUES ('Ayşe', 'ayse@example.com'), ('', 'adsiz@example.com'), ('Mehmet', 'mehmet@example.com')
			RETURNING id)
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) SELECT $1, id FROM people`,
		initialList); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

type queuedRow struct {
	email, status, error string
}

// rowsOf is a task's queue rows by recipient e-mail.
func rowsOf(t *testing.T, db *database.Store, task uuid.UUID) map[string]queuedRow {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(),
		`SELECT recipient_email, status::text, coalesce(error, '') FROM mail_queue WHERE task_id = $1`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]queuedRow{}
	for rows.Next() {
		var r queuedRow
		if err := rows.Scan(&r.email, &r.status, &r.error); err != nil {
			t.Fatal(err)
		}
		out[r.email] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, db *database.Store, query string) int {
	t.Helper()
	var n int
	if err := db.Conn.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// expectOneFailedRender checks a send to Ayşe, the unnamed recipient and
// Mehmet: the two named are queued, the unnamed one is failed with the render
// error, and the send reads failed with one failed and two pending.
func expectOneFailedRender(t *testing.T, db *database.Store, task uuid.UUID) {
	t.Helper()
	rows := rowsOf(t, db, task)
	if len(rows) != 3 {
		t.Fatalf("queue rows = %v, want one per recipient", rows)
	}
	for _, email := range []string{"ayse@example.com", "mehmet@example.com"} {
		if rows[email].status != "pending" {
			t.Errorf("%s: %+v, want pending", email, rows[email])
		}
	}
	unnamed := rows["adsiz@example.com"]
	if unnamed.status != "failed" || !strings.HasPrefix(unnamed.error, "render ") {
		t.Errorf("unnamed recipient: %+v, want failed with the render error", unnamed)
	}
	var status string
	if err := db.Conn.QueryRow(context.Background(), `SELECT mail_task_status($1)`, task).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("send status = %q, want failed: a recipient was not sent", status)
	}
}

func TestOneRecipientsRenderErrorDoesNotDropTheSend(t *testing.T) {
	captureMailerLogs(t)
	db := initialQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()

	t.Run("a mailing list", func(t *testing.T) {
		template, list := initialTemplate, initialList
		task, err := m.Enqueue(ctx, database.CreateMailTaskParams{
			SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("Enqueue: %v, want the send queued without the unnamed recipient", err)
		}
		expectOneFailedRender(t, db, task)
	})

	t.Run("a Keycloak group", func(t *testing.T) {
		task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`),
			Recipients: []mailer.RecipientInfo{
				{FullName: "Ayşe", Email: "ayse@example.com"},
				{FullName: "", Email: "adsiz@example.com"},
				{FullName: "Mehmet", Email: "mehmet@example.com"},
			},
		})
		if err != nil {
			t.Fatalf("EnqueueWithRecipients: %v, want the send queued without the unnamed recipient", err)
		}
		expectOneFailedRender(t, db, task)
	})

	t.Run("an approved send, in the caller's transaction", func(t *testing.T) {
		tx, err := db.Conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		template, list := initialTemplate, initialList
		task, err := m.Queue(db.WithTx(tx), mailer.PriorityBulk).Enqueue(ctx, database.CreateMailTaskParams{
			SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("Queue.Enqueue: %v, want the send queued without the unnamed recipient", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		expectOneFailedRender(t, db, task)
	})
}

// A single mail has one recipient: a mail that does not render is the whole
// send failing, and the caller (a login code, a Forms confirmation) is told,
// as before. Nothing of it is left behind: no send without queue rows.
func TestSingleMailThatDoesNotRenderIsRefusedAndLeavesNothing(t *testing.T) {
	captureMailerLogs(t)
	db := initialQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	tasks := count(t, db, `SELECT count(*) FROM mail_tasks`)

	template := initialTemplate
	task, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
		RecipientFullName: "", RecipientEmail: "adsiz@example.com",
	})
	if err == nil {
		t.Fatalf("EnqueueSingle queued %s, want the render error", task)
	}
	if !strings.Contains(err.Error(), "render ") {
		t.Errorf("EnqueueSingle error = %v, want the render error", err)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_tasks`); n != tasks {
		t.Errorf("mail_tasks = %d after the refused single mail, want %d: an orphan send was left", n, tasks)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_queue`); n != 0 {
		t.Errorf("mail_queue = %d rows, want none", n)
	}

	// The same mail to a named recipient is queued.
	if _, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
		RecipientFullName: "Ayşe", RecipientEmail: "ayse@example.com",
	}); err != nil {
		t.Fatalf("EnqueueSingle to a named recipient: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_queue WHERE status = 'pending'`); n != 1 {
		t.Errorf("pending rows = %d, want 1", n)
	}
}
