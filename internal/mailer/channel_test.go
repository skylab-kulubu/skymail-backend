package mailer_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// channelLog is which sender each row went out through.
type channelLog struct {
	mu   sync.Mutex
	sent map[uuid.UUID][]string
}

func newChannelLog() *channelLog { return &channelLog{sent: map[uuid.UUID][]string{}} }

// sender records rows as sent through the named sender.
func (l *channelLog) sender(name string) func(context.Context, database.MailQueue) error {
	return func(_ context.Context, job database.MailQueue) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.sent[job.ID] = append(l.sent[job.ID], name+"/"+job.Channel)
		return nil
	}
}

func (l *channelLog) of(id uuid.UUID) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sent[id]...)
}

// channelQueue is a migrated database with one send and nothing queued, its
// channel check dropped: the queue as a newer image, with more channels than
// this one knows, leaves it.
func channelQueue(t *testing.T) (*database.Store, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `ALTER TABLE mail_queue DROP CONSTRAINT mail_queue_channel_check`); err != nil {
		t.Fatal(err)
	}
	var task uuid.UUID
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', '{}')
		RETURNING id`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool), task
}

// queueOn queues a row of task on channel, due ago in the past.
func queueOn(t *testing.T, db *database.Store, task uuid.UUID, channel string, ago time.Duration) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.Conn.QueryRow(context.Background(), `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, channel, next_attempt_at)
		VALUES ($1, 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', $2, NOW() - $3 * INTERVAL '1 second')
		RETURNING id`, task, channel, int(ago.Seconds())).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type channelRow struct {
	status, error string
	attempts      int
}

func channelRowOf(t *testing.T, db *database.Store, id uuid.UUID) channelRow {
	t.Helper()
	var row channelRow
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT status::text, coalesce(error, ''), attempts FROM mail_queue WHERE id = $1`, id).
		Scan(&row.status, &row.error, &row.attempts); err != nil {
		t.Fatal(err)
	}
	return row
}

// A row queued before the channel, or by an image from before it, has no
// channel of its own: it is an e-mail, and goes out through the e-mail sender.
func TestARowQueuedWithoutAChannelGoesOutAsEmail(t *testing.T) {
	captureMailerLogs(t)
	db, ids := pendingQueue(t, 3)
	log := newChannelLog()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, log.sender("email"))
	mailer.SetChannelSender(m, "test", log.sender("test"))
	startMailer(t, m, 2)
	m.Wake()

	waitFor(t, "every row to be sent", 10*time.Second, func() bool {
		for _, id := range ids {
			if channelRowOf(t, db, id).status != "sent" {
				return false
			}
		}
		return true
	})
	for _, id := range ids {
		if got := log.of(id); len(got) != 1 || got[0] != "email/email" {
			t.Errorf("row %s went out through %v, want the e-mail sender once", id, got)
		}
	}
}

// The worker hands each row to its own channel's sender.
func TestTheWorkerSendsEachRowThroughItsChannel(t *testing.T) {
	captureMailerLogs(t)
	db, task := channelQueue(t)
	email := queueOn(t, db, task, "email", time.Minute)
	other := queueOn(t, db, task, "test", time.Minute)
	log := newChannelLog()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, log.sender("email"))
	mailer.SetChannelSender(m, "test", log.sender("test"))
	startMailer(t, m, 2)
	m.Wake()

	waitFor(t, "both rows to be sent", 10*time.Second, func() bool {
		return channelRowOf(t, db, email).status == "sent" && channelRowOf(t, db, other).status == "sent"
	})
	if got := log.of(email); len(got) != 1 || got[0] != "email/email" {
		t.Errorf("the e-mail went out through %v, want the e-mail sender once", got)
	}
	if got := log.of(other); len(got) != 1 || got[0] != "test/test" {
		t.Errorf("the test channel's row went out through %v, want the test sender once", got)
	}
}

// A row of a channel this image has no sender for (a newer image queued it,
// and this one was rolled back to) fails for good at once, saying why, and
// the worker goes on to the next row.
func TestARowOfAnUnknownChannelFailsForGoodAndTheWorkerGoesOn(t *testing.T) {
	captureMailerLogs(t)
	db, task := channelQueue(t)
	unknown := queueOn(t, db, task, "push", 2*time.Minute)
	next := queueOn(t, db, task, "email", time.Minute)
	log := newChannelLog()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, log.sender("email"))
	startMailer(t, m, 1)
	m.Wake()

	waitFor(t, "the e-mail after it to be sent", 10*time.Second, func() bool {
		return channelRowOf(t, db, next).status == "sent"
	})
	row := channelRowOf(t, db, unknown)
	if row.status != "failed" || row.attempts != 1 || !strings.Contains(row.error, `"push"`) {
		t.Errorf("the push row = %+v, want failed for good after one attempt, naming its channel", row)
	}
	if got := log.of(unknown); len(got) != 0 {
		t.Errorf("the push row went out through %v, want no sender", got)
	}
}

// Every queued row is an e-mail, and carries the recipient's Keycloak subject
// when the send knew it: a group's member does; a list's recipient and a
// single mail (POST /mail_tasks/single takes an address) do not. A recipient whose mail did not render is
// written failed with theirs too.
func TestQueuedRowsCarryTheirChannelAndTheRecipientsSubject(t *testing.T) {
	captureMailerLogs(t)
	db := initialQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()
	ayse := uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10")
	unnamed := uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a11")

	group, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`),
		Recipients: []mailer.RecipientInfo{
			{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayse},
			{FullName: "", Email: "adsiz@example.com", Sub: &unnamed},
			{FullName: "Mehmet", Email: "mehmet@example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	template, list := initialTemplate, initialList
	listed, err := m.Enqueue(ctx, database.CreateMailTaskParams{
		SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	single, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
		RecipientFullName: "Ayşe", RecipientEmail: "ayse@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	type queued struct{ channel, sub, status string }
	rowsOf := func(task uuid.UUID) map[string]queued {
		t.Helper()
		rows, err := db.Conn.Query(ctx, `
			SELECT recipient_email, channel, coalesce(recipient_sub::text, ''), status::text
			FROM mail_queue WHERE task_id = $1`, task)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]queued{}
		for rows.Next() {
			var email string
			var q queued
			if err := rows.Scan(&email, &q.channel, &q.sub, &q.status); err != nil {
				t.Fatal(err)
			}
			out[email] = q
		}
		return out
	}

	want := map[uuid.UUID]map[string]queued{
		group: {
			"ayse@example.com":   {"email", ayse.String(), "pending"},
			"adsiz@example.com":  {"email", unnamed.String(), "failed"},
			"mehmet@example.com": {"email", "", "pending"},
		},
		listed: {
			"ayse@example.com":   {"email", "", "pending"},
			"adsiz@example.com":  {"email", "", "failed"},
			"mehmet@example.com": {"email", "", "pending"},
		},
		single: {"ayse@example.com": {"email", "", "pending"}},
	}
	for task, wantRows := range want {
		got := rowsOf(task)
		if len(got) != len(wantRows) {
			t.Errorf("send %s: rows %+v, want %+v", task, got, wantRows)
			continue
		}
		for email, w := range wantRows {
			if got[email] != w {
				t.Errorf("send %s, %s: %+v, want %+v", task, email, got[email], w)
			}
		}
	}
}
