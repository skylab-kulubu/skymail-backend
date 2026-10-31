package mailer_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// A send that keeps no record (mail_tasks.erase_after_send, 20261031230000):
// core's account deletion notices go to the person after SkyMail has run
// their Erasure command, so nothing would ever erase them. The row goes out
// as written and is erased the moment its outcome is: what Account erasure
// leaves of mail to an erased person, no more.

const eraseAfterSendAddress = "silinen.kisi@example.com"

type keptRow struct {
	status, name, email, subject, body, html, sub, variables string
	sentAt                                                   bool
}

func keptRowOf(t *testing.T, db *database.Store, task uuid.UUID) keptRow {
	t.Helper()
	var row keptRow
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT coalesce(q.status::text, ''), q.recipient_full_name, q.recipient_email, q.subject, q.body,
		       coalesce(q.body_html, ''), coalesce(q.recipient_sub::text, ''), t.body_variables::text, q.sent_at IS NOT NULL
		FROM mail_queue q JOIN mail_tasks t ON t.id = q.task_id
		WHERE q.task_id = $1`, task).
		Scan(&row.status, &row.name, &row.email, &row.subject, &row.body, &row.html, &row.sub, &row.variables, &row.sentAt); err != nil {
		t.Fatal(err)
	}
	return row
}

func (r keptRow) erased() bool {
	return r.name == "Silinmiş kullanıcı" && r.email == "" && r.subject == "" && r.body == "" && r.html == "" &&
		r.sub == "" && r.variables == "{}"
}

func enqueueNotice(t *testing.T, m mailer.Mailer, erase bool) uuid.UUID {
	t.Helper()
	template := laneTemplate
	task, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{"Deadline":"30.11.2026"}`),
		RecipientFullName: "Silinen Kişi", RecipientEmail: eraseAfterSendAddress, EraseAfterSend: erase,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// deliveries records what each row looked like when it reached the relay.
type deliveries struct {
	mu   sync.Mutex
	jobs []database.MailQueue
	err  error
}

func (d *deliveries) deliver(_ context.Context, job database.MailQueue) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobs = append(d.jobs, job)
	return d.err
}

func (d *deliveries) all() []database.MailQueue {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]database.MailQueue(nil), d.jobs...)
}

// The mail goes out whole, to the person, and then its row names no one:
// no address, no name, no rendered mail, no variables. The row, its status
// and when it was sent stay, so the send's counts do.
func TestASendThatKeepsNoRecordIsErasedOnceSent(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 0)
	sent := &deliveries{}
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, sent.deliver)
	notice := enqueueNotice(t, m, true)
	kept := enqueueNotice(t, m, false)
	if before := keptRowOf(t, db, notice); before.erased() || before.email != eraseAfterSendAddress {
		t.Fatalf("queued row %+v: erased before it went out", before)
	}
	startMailer(t, m, 1)
	m.Wake()

	waitFor(t, "both mails to be sent", 10*time.Second, func() bool {
		return keptRowOf(t, db, notice).status == "sent" && keptRowOf(t, db, kept).status == "sent"
	})
	for _, job := range sent.all() {
		if job.RecipientEmail != eraseAfterSendAddress || job.Subject != "Merhaba Silinen Kişi" || job.Body == "" {
			t.Errorf("relay got %q %q %q, want the mail as written", job.RecipientEmail, job.Subject, job.Body)
		}
	}
	if got := len(sent.all()); got != 2 {
		t.Fatalf("relay got %d mails, want 2", got)
	}
	if row := keptRowOf(t, db, notice); !row.erased() || !row.sentAt {
		t.Errorf("sent row %+v: want erased, with its sent_at", row)
	}
	if row := keptRowOf(t, db, kept); row.erased() || row.email != eraseAfterSendAddress || row.variables == "{}" {
		t.Errorf("another send's row %+v: want it kept as it was", row)
	}
}

// A mail the relay refuses for good is erased too, and its address is not
// put on the suppression list: the refusal is recorded by its codes alone.
func TestASendThatKeepsNoRecordIsErasedWhenItFailsForGood(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 0)
	refused := &deliveries{err: mailer.SMTPReply("RCPT TO", 550, "5.1.1")}
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, refused.deliver)
	notice := enqueueNotice(t, m, true)
	startMailer(t, m, 1)
	m.Wake()

	waitFor(t, "the mail to fail", 10*time.Second, func() bool { return keptRowOf(t, db, notice).status == "failed" })
	if row := keptRowOf(t, db, notice); !row.erased() {
		t.Errorf("failed row %+v: want erased", row)
	}
	var suppressed int
	if err := db.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_suppressions`).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if suppressed != 0 {
		t.Errorf("%d suppressions written; the erased row's address is gone", suppressed)
	}
	var reason string
	if err := db.Conn.QueryRow(context.Background(), `SELECT coalesce(error, '') FROM mail_queue WHERE task_id = $1`, notice).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "smtp_rejected: RCPT TO 550 5.1.1" {
		t.Errorf("error %q, want the stage and codes kept", reason)
	}
}

// Until its outcome is written the row keeps what a retry sends: a row put
// back to pending after a passing failure is the mail as written. A row
// written failed when it is queued (a reserved domain, a suppressed address,
// a mail that does not render) is erased as it is written; an error that
// could quote the address goes with it, a code stays.
func TestASendThatKeepsNoRecordKeepsItsMailUntilItsOutcome(t *testing.T) {
	ctx := context.Background()
	db := laneQueue(t, 0)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	notice := enqueueNotice(t, m, true)
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'processing' WHERE task_id = $1`, notice); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'pending', attempts = 1, error = 'smtp_deferred: RCPT TO 450 4.2.1' WHERE task_id = $1`, notice); err != nil {
		t.Fatal(err)
	}
	if row := keptRowOf(t, db, notice); row.erased() || row.email != eraseAfterSendAddress {
		t.Errorf("row put back to pending %+v: want the mail kept for its retry", row)
	}

	if _, err := db.Conn.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error)
		VALUES ($1, 'Silinen Kişi', 'ikinci@example.invalid', 'Konu', 'Gövde', 'failed', $2)`, notice, reservedDomainError); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error)
		VALUES ($1, 'Silinen Kişi', 'ucuncu@example.com', 'Konu', 'Gövde', 'failed', 'mail: invalid address "ucuncu@example.com"')`, notice); err != nil {
		t.Fatal(err)
	}
	var quoted int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status = 'failed' AND error IS NULL`, notice).Scan(&quoted); err != nil {
		t.Fatal(err)
	}
	if quoted != 1 {
		t.Errorf("%d failed rows lost their error, want the one that quoted the address", quoted)
	}
	var left int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND recipient_email <> ''`, notice).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d rows keep an address, want only the pending one", left)
	}
	var reason string
	if err := db.Conn.QueryRow(ctx, `SELECT error FROM mail_queue WHERE task_id = $1 AND status = 'failed' AND error IS NOT NULL`, notice).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != reservedDomainError {
		t.Errorf("error %q, want it kept: mail_task_status tells a skipped row by it", reason)
	}
}
