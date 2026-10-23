package mailer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// unsubscribeQueue is suppressionQueue with Zeynep out of the duyuru stream,
// and Mehmet, whose address already bounced, out of it too.
func unsubscribeQueue(t *testing.T) *database.Store {
	t.Helper()
	db := suppressionQueue(t)
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO mail_unsubscribes (address, stream, source) VALUES
		    ('zeynep@example.com', 'duyuru', 'link'),
		    ('mehmet@example.com', 'duyuru', 'link')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueueRecipients(t *testing.T, m mailer.Transactional, template uuid.UUID, recipients ...mailer.RecipientInfo) uuid.UUID {
	t.Helper()
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: template, BodyVariables: []byte(`{"code":"123456"}`), Recipients: recipients,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func enqueueOne(t *testing.T, m mailer.Transactional, template uuid.UUID, email string) uuid.UUID {
	t.Helper()
	task, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{"code":"123456"}`),
		RecipientFullName: "Alıcı", RecipientEmail: email,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// A list send (the duyuru stream) to an address that left it writes that
// recipient failed with suppressed: unsubscribed and nothing tried; the
// others are queued, and the send reads as they do. The address is matched
// whatever its case; the log does not name it.
func TestAnUnsubscribedRecipientIsSkippedInTheDuyuruStream(t *testing.T) {
	logs := captureMailerLogs(t)
	db := unsubscribeQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)

	task := enqueueRecipients(t, m, initialTemplate,
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com"},
		mailer.RecipientInfo{FullName: "Zeynep", Email: " Zeynep@Example.com"})
	if got := rowTo(t, db, task, "ayse@example.com"); got.status != "pending" {
		t.Errorf("Ayşe: %+v, want pending", got)
	}
	if got := rowTo(t, db, task, " Zeynep@Example.com"); got.status != "failed" || got.error != "suppressed: unsubscribed" || got.attempts != 0 {
		t.Errorf("Zeynep: %+v, want failed with suppressed: unsubscribed and nothing tried", got)
	}
	if status := sendStatus(t, db, task); status != "sending" {
		t.Errorf("send status = %q, want sending: the unsubscribed recipient is skipped", status)
	}

	logs.mu.Lock()
	logged := logs.buf.String()
	logs.mu.Unlock()
	if !strings.Contains(logged, "left the duyuru stream") {
		t.Errorf("the log does not say a recipient left the stream:\n%s", logged)
	}
	if strings.Contains(strings.ToLower(logged), "zeynep") {
		t.Errorf("the log names the address:\n%s", logged)
	}
}

// The record holds the duyuru stream only: a single mail (the bilgi lane: a
// Forms confirmation) and account mail, even sent as a list, go to the
// address as before.
func TestOtherStreamsStillReachAnUnsubscribedAddress(t *testing.T) {
	db := unsubscribeQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)

	if got := rowTo(t, db, enqueueOne(t, m, formsTemplate, "zeynep@example.com"), "zeynep@example.com"); got.status != "pending" {
		t.Errorf("a Forms confirmation: %+v, want pending", got)
	}
	if got := rowTo(t, db, enqueueOne(t, m, accountTemplate, "zeynep@example.com"), "zeynep@example.com"); got.status != "pending" {
		t.Errorf("a login code: %+v, want pending", got)
	}
	task := enqueueRecipients(t, m, accountTemplate, mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"})
	if got := rowTo(t, db, task, "zeynep@example.com"); got.status != "pending" {
		t.Errorf("account mail sent as a list: %+v, want pending: account mail is never in the duyuru stream", got)
	}
}

// An address both bounced and unsubscribed is held by both: the bounce in
// every stream, the unsubscribe in the duyuru stream once the bounce is
// removed.
func TestABounceAndAnUnsubscribeHoldTogether(t *testing.T) {
	db := unsubscribeQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mehmet := mailer.RecipientInfo{FullName: "Mehmet", Email: "mehmet@example.com"}

	if got := rowTo(t, db, enqueueRecipients(t, m, initialTemplate, mehmet), "mehmet@example.com"); got.status != "failed" || !strings.HasPrefix(got.error, "suppressed: ") {
		t.Errorf("a list send: %+v, want failed, suppressed", got)
	}
	if got := rowTo(t, db, enqueueOne(t, m, formsTemplate, "mehmet@example.com"), "mehmet@example.com"); got.error != "suppressed: dsn 5.1.10" {
		t.Errorf("a Forms confirmation: %+v, want the bounce", got)
	}

	if _, err := db.Conn.Exec(context.Background(), `DELETE FROM mail_suppressions WHERE address = 'mehmet@example.com'`); err != nil {
		t.Fatal(err)
	}
	if got := rowTo(t, db, enqueueRecipients(t, m, initialTemplate, mehmet), "mehmet@example.com"); got.status != "failed" || got.error != "suppressed: unsubscribed" {
		t.Errorf("a list send without the bounce: %+v, want the unsubscribe", got)
	}
	if got := rowTo(t, db, enqueueOne(t, m, formsTemplate, "mehmet@example.com"), "mehmet@example.com"); got.status != "pending" {
		t.Errorf("a Forms confirmation without the bounce: %+v, want pending", got)
	}
}

// An address that leaves after its list mail was queued gets none of it: the
// dispatcher reads the record as it takes the bulk rows, writes that row
// failed with nothing tried, and neither sends it nor counts it in the send
// budget. Account mail queued in the bulk lane is taken all the same. With
// and without a budget.
func TestAnAddressThatLeftAfterQueueingGetsNoMail(t *testing.T) {
	for name, budget := range map[string]*mailer.RateBudget{"without a budget": nil, "with a budget": {PerMinute: 25}} {
		t.Run(name, func(t *testing.T) {
			captureMailerLogs(t)
			db := suppressionQueue(t)
			var m mailer.Transactional
			if budget == nil {
				m = mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
				mailer.PrepareTurns(m, 10)
			} else {
				m = budgeted(t, db, *budget, newTestClock(), 10)
			}
			list := enqueueRecipients(t, m, initialTemplate,
				mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com"},
				mailer.RecipientInfo{FullName: "Deniz", Email: "Deniz@example.com"})
			account := enqueueRecipients(t, m, accountTemplate, mailer.RecipientInfo{FullName: "Deniz", Email: "deniz@example.com"})
			if got := rowTo(t, db, list, "Deniz@example.com"); got.status != "pending" {
				t.Fatalf("Deniz before leaving: %+v, want pending", got)
			}
			if _, err := db.Conn.Exec(context.Background(),
				`INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('deniz@example.com', 'duyuru', 'link')`); err != nil {
				t.Fatal(err)
			}

			taken := mailer.Turn(context.Background(), m)
			got := map[string]bool{}
			for _, row := range taken {
				got[row.TaskID.String()+" "+row.RecipientEmail] = true
			}
			if len(taken) != 2 || !got[list.String()+" ayse@example.com"] || !got[account.String()+" deniz@example.com"] {
				t.Errorf("taken %v, want Ayşe's list mail and Deniz's account mail", got)
			}
			if row := rowTo(t, db, list, "Deniz@example.com"); row.status != "failed" || row.error != "suppressed: unsubscribed" || row.attempts != 0 {
				t.Errorf("Deniz's list mail: %+v, want failed with suppressed: unsubscribed and nothing tried", row)
			}
			if budget != nil {
				if n := count(t, db, `SELECT coalesce(sum(taken), 0)::int FROM mail_send_windows WHERE lane = 0 AND span_seconds = 60`); n != 2 {
					t.Errorf("bulk rows counted in the budget = %d, want the two taken", n)
				}
			}
			if again := mailer.Turn(context.Background(), m); len(again) != 0 {
				t.Errorf("a second turn took %d rows", len(again))
			}
		})
	}
}
