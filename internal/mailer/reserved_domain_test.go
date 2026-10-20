package mailer_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

const reservedDomainError = "reserved_domain: not sent, the address is in a domain reserved by RFC 2606/6761"

// A recipient in a reserved domain (a sandbox seed account at example.com) is
// written failed when the send is queued, with nothing tried: it never
// reaches the relay, which can only refuse it, nor the send budget. The
// send's other recipients are queued, and the send reads as they do: the
// reserved recipient is skipped, not failed (mail_task_status). A send to no
// one but reserved recipients reads failed: nothing went out.
func TestARecipientInAReservedDomainIsFailedWhenQueued(t *testing.T) {
	captureMailerLogs(t)
	db := initialQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn,
		mailer.WithReservedDomainsRefused(), mailer.WithRateBudget(mailer.RateBudget{PerMinute: 3, PerDay: 300}))
	ctx := context.Background()
	recipients := []mailer.RecipientInfo{
		{FullName: "Ayşe", Email: "ayse@yildizskylab.com"},
		{FullName: "Seed", Email: "seed-bot@example.com"},
	}
	check := func(t *testing.T, rows map[string]queuedRow) {
		t.Helper()
		if len(rows) != 2 {
			t.Fatalf("queue rows = %v, want one per recipient", rows)
		}
		if got := rows["ayse@yildizskylab.com"]; got.status != "pending" || got.error != "" {
			t.Errorf("deliverable recipient: %+v, want pending", got)
		}
		if got := rows["seed-bot@example.com"]; got.status != "failed" || got.error != reservedDomainError {
			t.Errorf("reserved recipient: %+v, want failed with %q", got, reservedDomainError)
		}
	}

	t.Run("a Keycloak group", func(t *testing.T) {
		task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Recipients: recipients,
		})
		if err != nil {
			t.Fatalf("EnqueueWithRecipients: %v", err)
		}
		check(t, rowsOf(t, db, task))
		if got := sendStatus(t, db, task); got != "sending" {
			t.Errorf("send status = %q, want sending: the reserved recipient is skipped, Ayşe is still pending", got)
		}
		if n := count(t, db, `SELECT count(*) FROM mail_queue WHERE recipient_email = 'seed-bot@example.com' AND attempts <> 0`); n != 0 {
			t.Errorf("%d reserved rows with an attempt, want none: nothing was tried", n)
		}
	})

	t.Run("in the caller's transaction", func(t *testing.T) {
		tx, err := db.Conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		task, err := m.Queue(db.WithTx(tx), mailer.PriorityBulk).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Recipients: recipients,
		})
		if err != nil {
			t.Fatalf("Queue.EnqueueWithRecipients: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		check(t, rowsOf(t, db, task))
	})

	t.Run("a single mail is accepted and failed, not refused", func(t *testing.T) {
		template := initialTemplate
		task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
			RecipientFullName: "Seed", RecipientEmail: "seed-bot@example.com",
		})
		if err != nil {
			t.Fatalf("EnqueueSingle: %v, want the send accepted with its recipient failed", err)
		}
		rows := rowsOf(t, db, task)
		if got := rows["seed-bot@example.com"]; len(rows) != 1 || got.status != "failed" || got.error != reservedDomainError {
			t.Errorf("single mail rows = %+v, want one failed with %q", rows, reservedDomainError)
		}
		if got := sendStatus(t, db, task); got != "failed" {
			t.Errorf("send status = %q, want failed: its one recipient was skipped, nothing went out", got)
		}
	})

	t.Run("the send budget counts only the deliverable rows", func(t *testing.T) {
		mailer.PrepareTurns(m, 3)
		taken := mailer.Turn(ctx, m)
		for _, row := range taken {
			if mailer.ReservedDomain(row.RecipientEmail) {
				t.Errorf("the dispatcher took a reserved row")
			}
		}
		if len(taken) != 2 {
			t.Errorf("the dispatcher took %d rows, want the 2 deliverable ones", len(taken))
		}
		if n := count(t, db, `SELECT coalesce(sum(taken), 0)::int FROM mail_send_windows WHERE span_seconds = 60`); n != 2 {
			t.Errorf("the budget counted %d sends, want 2", n)
		}
	})
}

// Without WithReservedDomainsRefused a reserved address is queued like any
// other: the tests send to example.com through a fake relay.
func TestReservedDomainsAreQueuedWithoutTheOption(t *testing.T) {
	captureMailerLogs(t)
	db := initialQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`),
		Recipients: []mailer.RecipientInfo{{FullName: "Seed", Email: "seed-bot@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, task)["seed-bot@example.com"]; got.status != "pending" {
		t.Errorf("reserved recipient without the option: %+v, want pending", got)
	}
}

// sendStatus is a send's status as mail_task_status derives it.
func sendStatus(t *testing.T, db *database.Store, task uuid.UUID) string {
	t.Helper()
	var status string
	if err := db.Conn.QueryRow(context.Background(), `SELECT mail_task_status($1)`, task).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
