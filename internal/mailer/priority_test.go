package mailer_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The priority lanes (mail_queue.priority): a single mail a person is waiting
// for (a login code, a certificate, a Forms confirmation) is never held back
// by a send to a list or a group.

var (
	laneTemplate = uuid.MustParse("50000000-0000-4000-8000-000000000001")
	laneList     = uuid.MustParse("50000000-0000-4000-8000-000000000002")
)

const laneSender = "31ef736f-72da-4a40-8791-d523199cf9f0"

// laneQueue is a migrated database with a template and a mailing list of
// listSize recipients, and nothing queued.
func laneQueue(t *testing.T, listSize int) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ($1, 'Duyuru', 'Merhaba {{.FullName}}', '<p>{{.FullName}}</p>', '{{.FullName}}', '');
		`, laneTemplate); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `INSERT INTO mailing_lists (id, name) VALUES ($1, 'Tüm üyeler')`, laneList); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		WITH people AS (
			INSERT INTO recipients (full_name, email)
			SELECT 'Üye ' || n, 'uye' || n || '@example.com' FROM generate_series(1, $2::int) n
			RETURNING id)
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) SELECT $1, id FROM people`,
		laneList, listSize); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

func enqueueList(t *testing.T, m mailer.Mailer) uuid.UUID {
	t.Helper()
	template, list := laneTemplate, laneList
	task, err := m.Enqueue(context.Background(), database.CreateMailTaskParams{
		SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func enqueueSingle(t *testing.T, m mailer.Mailer, email string) uuid.UUID {
	t.Helper()
	template := laneTemplate
	task, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{"Code":"123456"}`),
		RecipientFullName: "Giriş Yapan", RecipientEmail: email,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// lanesOf counts a task's queue rows by priority.
func lanesOf(t *testing.T, db *database.Store, task uuid.UUID) map[int16]int {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(),
		`SELECT priority, count(*) FROM mail_queue WHERE task_id = $1 GROUP BY priority`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int16]int{}
	for rows.Next() {
		var priority int16
		var n int
		if err := rows.Scan(&priority, &n); err != nil {
			t.Fatal(err)
		}
		out[priority] = n
	}
	return out
}

func TestEachSendPathQueuesInItsLane(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 4)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	ctx := context.Background()

	list := enqueueList(t, m)
	group, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: laneTemplate, BodyVariables: []byte(`{}`),
		Recipients: []mailer.RecipientInfo{{FullName: "A", Email: "a@example.com"}, {FullName: "B", Email: "b@example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	single := enqueueSingle(t, m, "kod@example.com")

	// An approved send to people is queued one task per person, like
	// /mail_tasks/single, but it is an operator's send: the bulk lane.
	tx, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	template := laneTemplate
	approved, err := m.Queue(db.WithTx(tx), mailer.PriorityBulk).EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
		RecipientFullName: "Onaylı", RecipientEmail: "onayli@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		task uuid.UUID
		want map[int16]int
	}{
		{"list", list, map[int16]int{0: 4}},
		{"group", group, map[int16]int{0: 2}},
		{"single", single, map[int16]int{1: 1}},
		{"approved single", approved, map[int16]int{0: 1}},
	} {
		got := lanesOf(t, db, c.task)
		if len(got) != len(c.want) || got[0] != c.want[0] || got[1] != c.want[1] {
			t.Errorf("%s send: rows by priority %v, want %v", c.name, got, c.want)
		}
	}
}

// The dispatcher's first turn after a single mail is queued takes it, while a
// list send has hundreds of rows due: bulk rows get at most part of the
// workers, so one is always free for the system lane.
func TestFirstTurnTakesSingleMailBesideABulkSend(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 300)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	list := enqueueList(t, m)
	single := enqueueSingle(t, m, "kod@example.com")

	// Both enqueues woke the dispatcher; with no workers, what its first turn
	// takes stays taken.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mailer.StartWithoutWorkers(m, ctx, 3)

	var takenSingle, takenBulk int
	waitFor(t, "the first turn", 5*time.Second, func() bool {
		if err := db.Conn.QueryRow(context.Background(), `
			SELECT count(*) FILTER (WHERE task_id = $1), count(*) FILTER (WHERE task_id = $2)
			FROM mail_queue WHERE status = 'processing'`, single, list).Scan(&takenSingle, &takenBulk); err != nil {
			t.Fatal(err)
		}
		return takenSingle+takenBulk == 3
	})
	if takenSingle != 1 || takenBulk != 2 {
		t.Fatalf("first turn took %d single and %d bulk rows, want 1 and 2", takenSingle, takenBulk)
	}
}

// A list send is going out, every bulk send slow: a single mail queued in the
// middle reaches the relay at once, without waiting for any bulk send to
// finish, and bulk sends never take more than their share of the workers.
func TestSingleMailIsNotHeldBackByABulkSend(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 300)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)

	var (
		bulkInFlight, bulkPeak, bulkDone atomic.Int32
		singleAt                         atomic.Int64
		slow                             = make(chan struct{})
		closeSlow                        sync.Once
	)
	t.Cleanup(func() { closeSlow.Do(func() { close(slow) }) })
	mailer.SetDeliver(m, func(ctx context.Context, job database.MailQueue) error {
		if job.Priority > 0 {
			singleAt.Store(time.Now().UnixNano())
			return nil
		}
		n := bulkInFlight.Add(1)
		defer bulkInFlight.Add(-1)
		for {
			peak := bulkPeak.Load()
			if n <= peak || bulkPeak.CompareAndSwap(peak, n) {
				break
			}
		}
		select {
		case <-slow:
		case <-ctx.Done():
			return ctx.Err()
		}
		bulkDone.Add(1)
		return nil
	})
	enqueueList(t, m)
	startMailer(t, m, 3)
	waitFor(t, "the bulk send to start", 5*time.Second, func() bool { return bulkInFlight.Load() >= 2 })

	queued := time.Now()
	enqueueSingle(t, m, "kod@example.com")
	// The dispatcher's tick is 10 seconds; well within it means the enqueue's
	// wake-up took the row.
	waitFor(t, "the single mail", 3*time.Second, func() bool { return singleAt.Load() != 0 })
	if done := bulkDone.Load(); done != 0 {
		t.Fatalf("%d bulk sends finished before the single mail went; it should not have waited for any", done)
	}
	t.Logf("single mail reached the relay %s after it was queued", time.Unix(0, singleAt.Load()).Sub(queued))

	// Let the list go: it still never holds more than its share.
	closeSlow.Do(func() { close(slow) })
	waitFor(t, "the list send", 30*time.Second, func() bool { return bulkDone.Load() == 300 })
	if peak := bulkPeak.Load(); peak > 2 {
		t.Fatalf("%d bulk sends at once, want at most 2 of 3 workers", peak)
	}
}

func TestBulkLaneLeavesASystemWorker(t *testing.T) {
	for workers, want := range map[int]int{1: 1, 2: 1, 3: 2, 4: 2, 5: 3, 10: 7} {
		if got := mailer.BulkSlotsFor(workers); got != want {
			t.Errorf("BulkSlotsFor(%d) = %d, want %d", workers, got, want)
		}
	}
}
