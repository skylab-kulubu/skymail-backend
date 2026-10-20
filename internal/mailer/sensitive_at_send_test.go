package mailer_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// A send's sensitivity is decided when it is queued (hub ticket
// iletisim-servisi 21): a template whose key is locked sensitive writes the
// copy 'true' whatever its row says, on each of the three ways a send is
// queued.

var (
	frozenList = uuid.MustParse("61000000-0000-4000-8000-000000000001")
	lockedTmpl = uuid.MustParse("61000000-0000-4000-8000-000000000002")
	shownTmpl  = uuid.MustParse("61000000-0000-4000-8000-000000000003")
	quietTmpl  = uuid.MustParse("61000000-0000-4000-8000-000000000004")
)

// frozenQueue is a migrated database with a list of one recipient and three
// templates: a locked key whose row says false (a row from before the lock,
// or written by hand), a shown one, and one that says nothing.
func frozenQueue(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		  VALUES ($1, 'event.ticket-created', 'Bilet', 'Biletin {{.code}}', '<p>{{.code}}</p>', '{{.code}}', '', false),
		         ($2, 'club.notice', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', '', false),
		         ($3, NULL, 'Serbest', 'Serbest', '<p>x</p>', 'x', '', NULL)`, []any{lockedTmpl, shownTmpl, quietTmpl}},
		{`INSERT INTO mailing_lists (id, name) VALUES ($1, 'Duyuru')`, []any{frozenList}},
		{`WITH person AS (INSERT INTO recipients (full_name, email) VALUES ('Ayşe Kaya', 'ayse@example.com') RETURNING id)
		  INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) SELECT $1, id FROM person`, []any{frozenList}},
	} {
		if _, err := postgres.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return database.NewStore(postgres.Pool)
}

func atSend(t *testing.T, db *database.Store, task uuid.UUID) string {
	t.Helper()
	var value string
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT content_sensitive_at_send FROM mail_tasks WHERE id = $1`, task).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestEveryWayToQueueFreezesALockedKeyAsSensitive(t *testing.T) {
	db := frozenQueue(t)
	m := mailer.NewMailer(db, mailer.SMTPConfig{}, mailer.SenderOn)
	ctx := context.Background()
	list := frozenList
	vars := []byte(`{"code":"482910"}`)

	for _, tc := range []struct {
		template uuid.UUID
		want     string
	}{
		{lockedTmpl, "true"},
		{shownTmpl, "false"},
		{quietTmpl, "unset"},
	} {
		template := tc.template
		listSend, err := m.Enqueue(ctx, database.CreateMailTaskParams{
			SentBy: "s", TemplateID: &template, MailListID: &list, BodyVariables: vars,
		})
		if err != nil {
			t.Fatal(err)
		}
		single, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy: "s", TemplateID: &template, BodyVariables: vars,
			RecipientFullName: "Ayşe Kaya", RecipientEmail: "ayse@example.com",
		})
		if err != nil {
			t.Fatal(err)
		}
		group, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: "s", TemplateID: template, MailListID: &list, BodyVariables: vars,
			Recipients: []mailer.RecipientInfo{{FullName: "Ayşe Kaya", Email: "ayse@example.com"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for path, task := range map[string]uuid.UUID{"list": listSend, "single": single, "recipients": group} {
			if got := atSend(t, db, task); got != tc.want {
				t.Errorf("template %s, %s send: content_sensitive_at_send %q, want %q", template, path, got, tc.want)
			}
		}
	}
}

// A send queued while its template's key was locked stays sensitive when the
// key changes and the row says false (#61 review D1): it reads sensitive and
// the content sweep masks it.
func TestALockedSendStaysSensitiveWhenItsTemplateLosesTheLock(t *testing.T) {
	db := frozenQueue(t)
	m := mailer.NewMailer(db, mailer.SMTPConfig{}, mailer.SenderOn)
	ctx := context.Background()
	list := frozenList
	task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: "s", TemplateID: lockedTmpl, MailListID: &list, BodyVariables: []byte(`{"code":"482910"}`),
		Recipients: []mailer.RecipientInfo{{FullName: "Ayşe Kaya", Email: "ayse@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []string{
		`UPDATE templates SET key = 'club.duyuru', content_sensitive = false
		 WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`,
		`UPDATE mail_tasks SET created_at = now() - interval '1 hour' WHERE id = $1`,
		`UPDATE mail_queue SET status = 'sent' WHERE task_id = $1`,
	} {
		if _, err := db.Conn.Exec(ctx, update, task); err != nil {
			t.Fatal(err)
		}
	}

	var sensitive bool
	if err := db.Conn.QueryRow(ctx, `SELECT mail_task_content_sensitive($1)`, task).Scan(&sensitive); err != nil {
		t.Fatal(err)
	}
	if !sensitive {
		t.Fatal("the send lost its sensitivity with its template's lock")
	}

	result, err := mailer.MaskSensitiveContent(ctx, db, mailer.MaskOptions{RetryAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sends != 1 || result.NotSensitive != 0 {
		t.Fatalf("sweep = %+v, want the send masked", result)
	}
	var subject string
	if err := db.Conn.QueryRow(ctx, `SELECT subject FROM mail_queue WHERE task_id = $1`, task).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject == "Biletin 482910" {
		t.Errorf("the code is still in the subject: %q", subject)
	}
}
