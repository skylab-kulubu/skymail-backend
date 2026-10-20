package mailer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// Templates whose key says what a send is: account mail (a system template,
// exempt), a Forms confirmation (suppressed) and a template an operator keyed
// account.* that is not a system template (suppressed: the key alone exempts
// nothing).
var (
	accountTemplate       = uuid.MustParse("61000000-0000-4000-8000-000000000001")
	formsTemplate         = uuid.MustParse("61000000-0000-4000-8000-000000000002")
	operatorAccountKeyed  = uuid.MustParse("61000000-0000-4000-8000-000000000003")
	operatorKeycloakKeyed = uuid.MustParse("61000000-0000-4000-8000-000000000004")
)

// suppressionQueue is initialQueue with the keyed templates and Mehmet's
// address suppressed by a report (5.1.10).
func suppressionQueue(t *testing.T) *database.Store {
	t.Helper()
	db := initialQueue(t)
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO templates (id, key, system, name, subject, html_content, plain_text_content, react_email_content) VALUES
		    ($1, 'keycloak.login-code', true, 'Giriş kodu', 'Kod', '<p>{{.code}}</p>', '{{.code}}', ''),
		    ($2, 'forms.response-received', false, 'Yanıt alındı', 'Yanıt', '<p>Alındı</p>', 'Alındı', ''),
		    ($3, 'account.x', false, 'Hesap', 'Hesap', '<p>Hesap</p>', 'Hesap', ''),
		    ($4, 'keycloak.x', false, 'Hesap', 'Hesap', '<p>Hesap</p>', 'Hesap', '')`,
		accountTemplate, formsTemplate, operatorAccountKeyed, operatorKeycloakKeyed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(context.Background(),
		`INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('mehmet@example.com', 'dsn', '5.1.10')`); err != nil {
		t.Fatal(err)
	}
	return db
}

type flaggedRow struct {
	status, error string
	attempts      int
	flagged       bool
}

func rowTo(t *testing.T, db *database.Store, task uuid.UUID, email string) flaggedRow {
	t.Helper()
	var r flaggedRow
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT status::text, coalesce(error, ''), attempts, suppressed_but_sent
		FROM mail_queue WHERE task_id = $1 AND lower(recipient_email) = lower($2)`, task, email).
		Scan(&r.status, &r.error, &r.attempts, &r.flagged); err != nil {
		t.Fatalf("row to %s: %v", email, err)
	}
	return r
}

// A recipient at a suppressed address is written failed when the send is
// queued, with the suppression's source and codes and nothing tried: it never
// reaches the relay or the send budget. The others are queued, and the send
// reads as they do (the suppressed recipient is skipped). The address is
// matched whatever its case; neither the row's error nor the log names it.
func TestASuppressedRecipientIsFailedWhenQueued(t *testing.T) {
	logs := captureMailerLogs(t)
	db := suppressionQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()

	task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`),
		Recipients: []mailer.RecipientInfo{
			{FullName: "Ayşe", Email: "ayse@example.com"},
			{FullName: "Mehmet", Email: " Mehmet@Example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowTo(t, db, task, "ayse@example.com"); got.status != "pending" || got.flagged {
		t.Errorf("Ayşe: %+v, want pending, not flagged", got)
	}
	got := rowTo(t, db, task, " Mehmet@Example.com")
	if got.status != "failed" || got.error != "suppressed: dsn 5.1.10" || got.attempts != 0 {
		t.Errorf("Mehmet: %+v, want failed with suppressed: dsn 5.1.10 and nothing tried", got)
	}
	if status := sendStatus(t, db, task); status != "sending" {
		t.Errorf("send status = %q, want sending: the suppressed recipient is skipped", status)
	}

	logs.mu.Lock()
	logged := logs.buf.String()
	logs.mu.Unlock()
	if !strings.Contains(logged, "suppressed") {
		t.Errorf("the log does not say a recipient was suppressed:\n%s", logged)
	}
	if strings.Contains(strings.ToLower(logged), "mehmet") {
		t.Errorf("the log names the address:\n%s", logged)
	}
}

// Only account mail (keycloak.*, account.*) goes to a suppressed address: it
// is queued, its row flagged, and the suppression says when. Any other single
// mail (a Forms confirmation) is accepted and written failed, and the send
// reads failed: nothing went out.
func TestOnlyAccountMailGoesToASuppressedAddress(t *testing.T) {
	db := suppressionQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()

	account := accountTemplate
	task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &account, BodyVariables: []byte(`{"code":"123456"}`),
		RecipientFullName: "Mehmet", RecipientEmail: "mehmet@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowTo(t, db, task, "mehmet@example.com"); got.status != "pending" || !got.flagged || got.error != "" {
		t.Errorf("account mail: %+v, want pending and flagged", got)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_suppressions WHERE exempt_flagged_at IS NOT NULL`); n != 1 {
		t.Errorf("suppressions flagged = %d, want 1", n)
	}

	forms := formsTemplate
	task, err = m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &forms, BodyVariables: []byte(`{}`),
		RecipientFullName: "Mehmet", RecipientEmail: "mehmet@example.com",
	})
	if err != nil {
		t.Fatalf("a Forms confirmation to a suppressed address was refused: %v", err)
	}
	if got := rowTo(t, db, task, "mehmet@example.com"); got.status != "failed" || got.error != "suppressed: dsn 5.1.10" || got.flagged {
		t.Errorf("Forms confirmation: %+v, want failed, suppressed", got)
	}
	if status := sendStatus(t, db, task); status != "failed" {
		t.Errorf("send status = %q, want failed: nothing went out", status)
	}
}

// The key alone exempts nothing: a template keyed account.* or keycloak.* that
// is not a system template (an operator's, made with skymail:templates:write)
// is suppressed like any other, so the key cannot be used to send past the
// list (review of skymail-backend #87).
func TestAnAccountKeyWithoutSystemIsSuppressed(t *testing.T) {
	db := suppressionQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()

	for _, template := range []uuid.UUID{operatorAccountKeyed, operatorKeycloakKeyed} {
		task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
			RecipientFullName: "Mehmet", RecipientEmail: "mehmet@example.com",
		})
		if err != nil {
			t.Fatalf("template %s: %v", template, err)
		}
		if got := rowTo(t, db, task, "mehmet@example.com"); got.status != "failed" || got.error != "suppressed: dsn 5.1.10" || got.flagged {
			t.Errorf("template %s: %+v, want failed, suppressed, not flagged", template, got)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM mail_suppressions WHERE exempt_flagged_at IS NOT NULL`); n != 0 {
		t.Errorf("suppressions flagged = %d, want 0", n)
	}
}

func TestAccountMailIsASystemTemplateWithAnAccountKey(t *testing.T) {
	for _, c := range []struct {
		key    string
		system bool
		want   bool
	}{
		{"keycloak.login-code", true, true},
		{"account.security-alert", true, true},
		{"account.x", false, false},
		{"keycloak.x", false, false},
		{"core.welcome", true, false},
		{"forms.response-received", false, false},
	} {
		key := c.key
		if got := mailer.AccountMail(&key, c.system); got != c.want {
			t.Errorf("AccountMail(%q, system %v) = %v, want %v", c.key, c.system, got, c.want)
		}
	}
	if mailer.AccountMail(nil, true) {
		t.Error("AccountMail(no template, system) = true, want false")
	}
}

// The ticket's acceptance (hub ticket skymail-bounce-isleme 05): after one
// permanent refusal, a second send to the address does not reach the relay;
// account mail still goes, flagged; once removed, the address is sent to
// again. A refusal that is not of the address suppresses nothing.
func TestOnePermanentRefusalStopsTheNextSend(t *testing.T) {
	db := suppressionQueue(t)
	ctx := context.Background()
	single := func(m mailer.Transactional, template uuid.UUID, email string) uuid.UUID {
		t.Helper()
		task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{"code":"123456"}`),
			RecipientFullName: "Zeynep", RecipientEmail: email,
		})
		if err != nil {
			t.Fatal(err)
		}
		m.Wake()
		return task
	}
	waitStatus := func(task uuid.UUID, email, status string) flaggedRow {
		t.Helper()
		var row flaggedRow
		waitFor(t, "the row to be "+status, 10*time.Second, func() bool {
			row = rowTo(t, db, task, email)
			return row.status == status
		})
		return row
	}

	// The relay refuses every recipient's address.
	refusing := mailer.NewMailer(db, answeringConfig(startAnsweringRelay(t, "RCPT", "550 5.1.1 <zeynep@example.com>: Recipient address rejected")), mailer.SenderOn)
	plainSMTP(refusing)
	refusingCtx, stopRefusing := context.WithCancel(ctx)
	defer stopRefusing()
	refusing.Start(refusingCtx, 1)
	first := single(refusing, formsTemplate, "Zeynep@Example.com")
	if got := waitStatus(first, "Zeynep@Example.com", "failed"); got.error != "smtp_rejected: RCPT TO 550 5.1.1" {
		t.Fatalf("first send: %+v", got)
	}
	var source, reason string
	waitFor(t, "the address to be suppressed", 10*time.Second, func() bool {
		return count(t, db, `SELECT count(*) FROM mail_suppressions WHERE address = 'zeynep@example.com'`) == 1
	})
	if err := db.Conn.QueryRow(ctx, `SELECT source, reason_code FROM mail_suppressions WHERE address = 'zeynep@example.com'`).
		Scan(&source, &reason); err != nil {
		t.Fatal(err)
	}
	if source != "smtp" || reason != "RCPT TO 550 5.1.1" {
		t.Errorf("suppression = %s %q, want smtp RCPT TO 550 5.1.1", source, reason)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces b JOIN mail_queue q ON q.id = b.queue_id
		WHERE q.task_id = '`+first.String()+`' AND b.source = 'smtp' AND b.kind = 'hard' AND b.status = '5.1.1' AND b.smtp_code = 550`); n != 1 {
		t.Errorf("refusal bounces = %d, want 1", n)
	}

	second := single(refusing, formsTemplate, "zeynep@example.com")
	if got := rowTo(t, db, second, "zeynep@example.com"); got.status != "failed" || got.error != "suppressed: smtp RCPT TO 550 5.1.1" || got.attempts != 0 {
		t.Errorf("second send: %+v, want failed when queued, nothing tried", got)
	}

	if err := refusing.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	stopRefusing()

	// Account mail goes through a relay that takes it, flagged.
	relay := startAnsweringRelay(t, "", "")
	taking := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(taking)
	startMailer(t, taking, 1)
	account := single(taking, accountTemplate, "zeynep@example.com")
	if got := waitStatus(account, "zeynep@example.com", "sent"); !got.flagged {
		t.Errorf("account mail: %+v, want sent and flagged", got)
	}

	// Removed, the address is sent to again.
	var id uuid.UUID
	if err := db.Conn.QueryRow(ctx, `SELECT id FROM mail_suppressions WHERE address = 'zeynep@example.com'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DeleteSuppression(ctx, id); err != nil {
		t.Fatal(err)
	}
	again := single(taking, formsTemplate, "zeynep@example.com")
	if got := waitStatus(again, "zeynep@example.com", "sent"); got.flagged {
		t.Errorf("after removal: %+v, want sent, not flagged", got)
	}
}

// A refusal that is not of the address (the relay's policy, a full mailbox)
// is tried again and suppresses nothing.
func TestATransientRefusalSuppressesNothing(t *testing.T) {
	db := suppressionQueue(t)
	m := mailer.NewMailer(db, answeringConfig(startAnsweringRelay(t, "RCPT", "550 5.7.1 Relay access denied")), mailer.SenderOn)
	plainSMTP(m)
	startMailer(t, m, 1)
	forms := formsTemplate
	task, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &forms, BodyVariables: []byte(`{}`),
		RecipientFullName: "Zeynep", RecipientEmail: "zeynep@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Wake()
	waitFor(t, "a retry", 10*time.Second, func() bool {
		return rowTo(t, db, task, "zeynep@example.com").attempts >= 1
	})
	if n := count(t, db, `SELECT count(*) FROM mail_suppressions WHERE address = 'zeynep@example.com'`); n != 0 {
		t.Errorf("a policy refusal suppressed the address")
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces`); n != 0 {
		t.Errorf("a policy refusal was recorded as a bounce")
	}
}
