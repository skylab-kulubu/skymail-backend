package mailer_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// relay stands in for SMTP: it counts the mail each process hands it, per
// queue row. A row handed over twice is a person getting the mail twice.
type relay struct {
	mu        sync.Mutex
	delivered map[uuid.UUID][]string
}

func newRelay() *relay { return &relay{delivered: map[uuid.UUID][]string{}} }

// deliverAfter hands a row over after a pause, as a slow relay would. A send
// the process gives up on (ctx ends first) never reaches the relay.
func (r *relay) deliverAfter(by string, pause time.Duration) func(context.Context, database.MailQueue) error {
	return func(ctx context.Context, job database.MailQueue) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.delivered[job.ID] = append(r.delivered[job.ID], by)
		return nil
	}
}

func (r *relay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, by := range r.delivered {
		n += len(by)
	}
	return n
}

func (r *relay) snapshot() map[uuid.UUID][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uuid.UUID][]string, len(r.delivered))
	for id, by := range r.delivered {
		out[id] = append([]string(nil), by...)
	}
	return out
}

// pendingQueue is a migrated database with n rows due now, pending.
func pendingQueue(t *testing.T, n int) (*database.Store, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	var task uuid.UUID
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', '{}')
		RETURNING id`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		if err := postgres.Pool.QueryRow(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, next_attempt_at)
			VALUES ($1, 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', NOW() - INTERVAL '1 minute')
			RETURNING id`, task).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	return database.NewStore(postgres.Pool), ids
}

type leaseRow struct {
	Status    string
	ClaimedBy string
	Attempts  int
}

func leaseRows(t *testing.T, db *database.Store) map[uuid.UUID]leaseRow {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(),
		`SELECT id, status::text, COALESCE(claimed_by, ''), attempts FROM mail_queue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]leaseRow{}
	for rows.Next() {
		var id uuid.UUID
		var row leaseRow
		if err := rows.Scan(&id, &row.Status, &row.ClaimedBy, &row.Attempts); err != nil {
			t.Fatal(err)
		}
		out[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, what string, timeout time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A start-first deploy: the old task is in the middle of the queue when the
// new task starts on the same database. Every row reaches the relay once.
func TestStartFirstOverlapSendsEveryRowOnce(t *testing.T) {
	captureMailerLogs(t)
	db, ids := pendingQueue(t, 24)
	relay := newRelay()
	// A worker makes its SMTP client at startup; deliver replaces the send.
	smtp := startFakeSMTP(t).config()

	old := mailer.NewMailer(db, smtp, mailer.SenderOn)
	mailer.SetDeliver(old, relay.deliverAfter("old", 150*time.Millisecond))
	startMailer(t, old, 3)
	old.Wake()

	// The old task has sent a few and holds the next ones.
	waitFor(t, "the old task to send", 10*time.Second, func() bool { return relay.count() >= 3 })
	var heldByOld int
	for _, row := range leaseRows(t, db) {
		if row.Status == "processing" && row.ClaimedBy == mailer.Claimant(old) {
			heldByOld++
		}
	}
	if heldByOld == 0 || heldByOld > 3 {
		t.Fatalf("old task holds %d rows, want 1 to 3: one per worker", heldByOld)
	}

	replacement := mailer.NewMailer(db, smtp, mailer.SenderOn)
	if mailer.Claimant(replacement) == mailer.Claimant(old) {
		t.Fatalf("two processes share the claimant %q", mailer.Claimant(old))
	}
	mailer.SetDeliver(replacement, relay.deliverAfter("new", 150*time.Millisecond))
	startMailer(t, replacement, 3)
	replacement.Wake()

	waitFor(t, "every row to be sent", 30*time.Second, func() bool {
		for _, row := range leaseRows(t, db) {
			if row.Status != "sent" {
				return false
			}
		}
		return true
	})
	// Anything a reset had wrongly put back would go out within a moment.
	time.Sleep(500 * time.Millisecond)

	delivered := relay.snapshot()
	for _, id := range ids {
		if by := delivered[id]; len(by) != 1 {
			t.Errorf("row %s reached the relay %d times (%v), want once", id, len(by), by)
		}
	}
	for id, row := range leaseRows(t, db) {
		if row.ClaimedBy != mailer.Claimant(old) && row.ClaimedBy != mailer.Claimant(replacement) {
			t.Errorf("sent row %s claimed by %q", id, row.ClaimedBy)
		}
	}
}

// A task that took rows and hangs (or died) keeps them for the lease: another
// task starting leaves them alone. Once the lease has run out a task starting
// takes them and sends them, and what the hung task records afterwards
// changes nothing.
func TestRowsOfAHungTaskGoToAnotherTaskOnlyAfterTheLease(t *testing.T) {
	logs := captureMailerLogs(t)
	db, ids := pendingQueue(t, 2)
	relay := newRelay()
	// A worker makes its SMTP client at startup; deliver replaces the send.
	smtp := startFakeSMTP(t).config()

	release := make(chan struct{})
	hung := mailer.NewMailer(db, smtp, mailer.SenderOn)
	mailer.SetDeliver(hung, func(ctx context.Context, job database.MailQueue) error {
		select {
		case <-release:
			return errors.New("421 relay closing connection")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	startMailer(t, hung, 3)
	hung.Wake()
	waitFor(t, "the hung task to take both rows", 10*time.Second, func() bool {
		rows := leaseRows(t, db)
		for _, id := range ids {
			if rows[id].Status != "processing" || rows[id].ClaimedBy != mailer.Claimant(hung) {
				return false
			}
		}
		return true
	})

	// Within the lease another task, starting, leaves them to the hung one.
	second := mailer.NewMailer(db, smtp, mailer.SenderOn)
	mailer.SetDeliver(second, relay.deliverAfter("second", 0))
	startMailer(t, second, 3)
	second.Wake()
	time.Sleep(time.Second)
	if n := relay.count(); n != 0 {
		t.Fatalf("rows within their lease reached the relay %d times", n)
	}
	for _, id := range ids {
		if row := leaseRows(t, db)[id]; row.Status != "processing" || row.ClaimedBy != mailer.Claimant(hung) {
			t.Fatalf("row %s within its lease = %+v, want the hung task's", id, row)
		}
	}

	// The lease runs out; the next task to start takes the rows and sends them.
	if _, err := db.Conn.Exec(context.Background(),
		`UPDATE mail_queue SET claimed_at = NOW() - INTERVAL '11 minutes'`); err != nil {
		t.Fatal(err)
	}
	third := mailer.NewMailer(db, smtp, mailer.SenderOn)
	mailer.SetDeliver(third, relay.deliverAfter("third", 0))
	startMailer(t, third, 3)
	waitFor(t, "both rows to be sent", 20*time.Second, func() bool {
		rows := leaseRows(t, db)
		return rows[ids[0]].Status == "sent" && rows[ids[1]].Status == "sent"
	})

	// The hung task's relay finally answers, with a failure it would retry.
	// The rows are no longer its own: nothing it records lands.
	close(release)
	waitFor(t, "the hung task to find its lease gone", 10*time.Second, func() bool {
		n := 0
		for _, line := range logs.lines(t) {
			if line["outcome"] == "retry" && line["claimant"] == mailer.Claimant(hung) {
				n++
			}
		}
		return n == 2
	})
	delivered := relay.snapshot()
	for _, id := range ids {
		if by := delivered[id]; len(by) != 1 {
			t.Errorf("row %s reached the relay %d times (%v), want once", id, len(by), by)
		}
		row := leaseRows(t, db)[id]
		if row.Status != "sent" || row.Attempts != 0 || row.ClaimedBy == mailer.Claimant(hung) {
			t.Errorf("row %s = %+v, want sent by another task with no failed attempt", id, row)
		}
	}
}
