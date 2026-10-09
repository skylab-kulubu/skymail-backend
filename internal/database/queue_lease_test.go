package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The queue lease: a row a process takes is claimed by it for a lease, put
// back only once the lease has run out, and finished only by the process that
// holds it.

const leaseSeconds = 600

// queueOf writes one mail task and n rows for it, all due now and pending, and
// returns the rows' ids in the order the dispatcher takes them.
func queueOf(t *testing.T, db *Store, n int) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var task uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', '{}')
		RETURNING id`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		if err := db.Conn.QueryRow(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, next_attempt_at)
			VALUES ($1, 'Alıcı', 'alici@example.com', 'Konu', 'Gövde', NOW() - make_interval(mins => $2))
			RETURNING id`, task, n-i).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

type claim struct {
	Status    string
	ClaimedBy *string
	ClaimedAt *time.Time
}

func claimOf(t *testing.T, db *Store, id uuid.UUID) claim {
	t.Helper()
	var c claim
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT status::text, claimed_by, claimed_at FROM mail_queue WHERE id = $1`, id).
		Scan(&c.Status, &c.ClaimedBy, &c.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	return c
}

// claimedAgo moves a row's claim back in time, as if it was taken that long ago.
func claimedAgo(t *testing.T, db *Store, id uuid.UUID, ago time.Duration) {
	t.Helper()
	if _, err := db.Conn.Exec(context.Background(),
		`UPDATE mail_queue SET claimed_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
		id, ago.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func TestClaimTakesAtMostMaxRowsAndLeasesThemToTheProcess(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	ids := queueOf(t, db, 5)

	taken, err := db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-a-1", MaxRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 2 {
		t.Fatalf("took %d rows, want 2", len(taken))
	}
	// The two due longest; RETURNING keeps no order.
	if !(taken[0].ID == ids[0] && taken[1].ID == ids[1]) && !(taken[0].ID == ids[1] && taken[1].ID == ids[0]) {
		t.Errorf("took %s and %s, want the two due longest, %s and %s", taken[0].ID, taken[1].ID, ids[0], ids[1])
	}
	for i, row := range taken {
		if row.ClaimedBy == nil || *row.ClaimedBy != "skymail-a-1" || row.ClaimedAt == nil {
			t.Errorf("row %d claim = %v at %v, want skymail-a-1 now", i, row.ClaimedBy, row.ClaimedAt)
		} else if age := time.Since(*row.ClaimedAt); age < -time.Minute || age > time.Minute {
			t.Errorf("row %d claimed_at %v is not now", i, row.ClaimedAt)
		}
		if row.Status.MailQueueStatus != MailQueueStatusProcessing {
			t.Errorf("row %d status = %v", i, row.Status)
		}
	}
	for _, id := range ids[2:] {
		if c := claimOf(t, db, id); c.Status != "pending" || c.ClaimedBy != nil || c.ClaimedAt != nil {
			t.Errorf("untaken row %s = %+v, want pending and unclaimed", id, c)
		}
	}

	// Another process skips what the first holds and takes the rest.
	rest, err := db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-b-2", MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 {
		t.Fatalf("second process took %d rows, want the 3 left", len(rest))
	}
}

// A start-first deploy: the old task is sending the rows it took when the new
// task starts. The new task's startup reset leaves them alone; only a row
// whose lease ran out goes back to pending.
func TestResetDeadJobsPutsBackOnlyRowsWhoseLeaseRanOut(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	ids := queueOf(t, db, 3)
	if _, err := db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-old-1", MaxRows: 3}); err != nil {
		t.Fatal(err)
	}
	fresh, almost, stale := ids[0], ids[1], ids[2]
	claimedAgo(t, db, almost, 9*time.Minute)
	claimedAgo(t, db, stale, 11*time.Minute)

	stamped, err := db.StartLeaseOnUnclaimedJobs(ctx)
	if err != nil || stamped != 0 {
		t.Fatalf("stamped %d rows (%v), want none: every row has a claim", stamped, err)
	}
	reset, err := db.ResetDeadJobs(ctx, leaseSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Fatalf("reset %d rows, want only the stale one", reset)
	}
	for _, id := range []uuid.UUID{fresh, almost} {
		if c := claimOf(t, db, id); c.Status != "processing" || c.ClaimedBy == nil || *c.ClaimedBy != "skymail-old-1" {
			t.Errorf("row within its lease = %+v, want still processing for skymail-old-1", c)
		}
	}
	if c := claimOf(t, db, stale); c.Status != "pending" || c.ClaimedBy != nil || c.ClaimedAt != nil {
		t.Errorf("stale row = %+v, want pending and unclaimed", c)
	}

	// The stale row is the next process's to take.
	taken, err := db.ProcessQueueItems(ctx, ProcessQueueItemsParams{ClaimedBy: "skymail-new-2", MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 || taken[0].ID != stale {
		t.Fatalf("new process took %v, want only the stale row", taken)
	}
}

// A processing row with no claim was taken by an image from before the lease
// (it may be sending it right now) or comes from an old dump. Its lease starts
// when a process first sees it; it goes back to pending one lease later.
func TestUnclaimedProcessingRowGetsALeaseBeforeItCanBeReset(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	ids := queueOf(t, db, 1)
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'processing' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	stamped, err := db.StartLeaseOnUnclaimedJobs(ctx)
	if err != nil || stamped != 1 {
		t.Fatalf("stamped %d rows (%v), want 1", stamped, err)
	}
	if reset, err := db.ResetDeadJobs(ctx, leaseSeconds); err != nil || reset != 0 {
		t.Fatalf("reset %d rows (%v) right after the stamp, want none", reset, err)
	}
	if c := claimOf(t, db, ids[0]); c.Status != "processing" || c.ClaimedAt == nil || c.ClaimedBy != nil {
		t.Fatalf("row = %+v, want processing with a lease start and no claimant", c)
	}

	// Stamping again does not move the lease on.
	claimedAgo(t, db, ids[0], 11*time.Minute)
	if stamped, err := db.StartLeaseOnUnclaimedJobs(ctx); err != nil || stamped != 0 {
		t.Fatalf("second stamp touched %d rows (%v)", stamped, err)
	}
	if reset, err := db.ResetDeadJobs(ctx, leaseSeconds); err != nil || reset != 1 {
		t.Fatalf("reset %d rows (%v) one lease later, want 1", reset, err)
	}
}

// takeOne claims the one due row for claimant and returns the claim.
func takeOne(t *testing.T, db *Store, claimant string) MailQueue {
	t.Helper()
	taken, err := db.ProcessQueueItems(context.Background(), ProcessQueueItemsParams{ClaimedBy: claimant, MaxRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 || taken[0].ClaimedAt == nil {
		t.Fatalf("%s took %v, want one claimed row", claimant, taken)
	}
	return taken[0]
}

// expire runs a row's lease out and lets the reset put it back.
func expire(t *testing.T, db *Store, id uuid.UUID) {
	t.Helper()
	claimedAgo(t, db, id, 11*time.Minute)
	if n, err := db.ResetDeadJobs(context.Background(), leaseSeconds); err != nil || n != 1 {
		t.Fatalf("reset %d rows (%v), want 1", n, err)
	}
}

// stale tries all three outcomes for an old claim and checks none lands.
func stale(t *testing.T, db *Store, old MailQueue) {
	t.Helper()
	ctx := context.Background()
	reason := "dial tcp: connection refused"
	if n, err := db.SetMailQueueItemSent(ctx, SetMailQueueItemSentParams{
		ID: old.ID, ClaimedBy: *old.ClaimedBy, ClaimedAt: *old.ClaimedAt,
	}); err != nil || n != 0 {
		t.Fatalf("stale sent touched %d rows (%v)", n, err)
	}
	if n, err := db.SetMailQueueItemFailed(ctx, SetMailQueueItemFailedParams{
		ID: old.ID, ClaimedBy: *old.ClaimedBy, ClaimedAt: *old.ClaimedAt, Error: &reason,
	}); err != nil || n != 0 {
		t.Fatalf("stale failed touched %d rows (%v)", n, err)
	}
	if _, err := db.RescheduleMailQueueItem(ctx, RescheduleMailQueueItemParams{
		ID: old.ID, ClaimedBy: *old.ClaimedBy, ClaimedAt: *old.ClaimedAt, Error: &reason, DelaySeconds: 30,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale reschedule err = %v, want no rows", err)
	}
}

func attemptsOf(t *testing.T, db *Store, id uuid.UUID) int {
	t.Helper()
	var attempts int
	if err := db.Conn.QueryRow(context.Background(), `SELECT attempts FROM mail_queue WHERE id = $1`, id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	return attempts
}

// A process whose lease ran out may still finish its send. What it then
// records must not overwrite the row another process has taken since.
func TestOnlyTheClaimHoldingARowRecordsItsOutcome(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	row := queueOf(t, db, 1)[0]

	first := takeOne(t, db, "skymail-a-1")
	expire(t, db, row)

	// Put back, nobody's yet: the old claim records nothing.
	stale(t, db, first)
	if c := claimOf(t, db, row); c.Status != "pending" {
		t.Fatalf("row after stale outcomes = %+v, want pending", c)
	}

	// Taken by another process: the old claim still records nothing.
	second := takeOne(t, db, "skymail-b-2")
	stale(t, db, first)
	if c := claimOf(t, db, row); c.Status != "processing" || c.ClaimedBy == nil || *c.ClaimedBy != "skymail-b-2" {
		t.Fatalf("row after the stale outcomes = %+v, want processing for skymail-b-2", c)
	}
	if n := attemptsOf(t, db, row); n != 0 {
		t.Fatalf("attempts = %d, want 0", n)
	}

	// The holder reschedules: back to pending, unclaimed, one attempt.
	reason := "421 try again later"
	attempts, err := db.RescheduleMailQueueItem(ctx, RescheduleMailQueueItemParams{
		ID: row, ClaimedBy: *second.ClaimedBy, ClaimedAt: *second.ClaimedAt, Error: &reason, DelaySeconds: 0,
	})
	if err != nil || attempts != 1 {
		t.Fatalf("reschedule = %d (%v), want 1", attempts, err)
	}
	if c := claimOf(t, db, row); c.Status != "pending" || c.ClaimedBy != nil || c.ClaimedAt != nil {
		t.Fatalf("rescheduled row = %+v, want pending and unclaimed", c)
	}

	// Taken again and sent by its holder: sent, and it keeps who sent it.
	third := takeOne(t, db, "skymail-b-2")
	if n, err := db.SetMailQueueItemSent(ctx, SetMailQueueItemSentParams{
		ID: row, ClaimedBy: *third.ClaimedBy, ClaimedAt: *third.ClaimedAt,
	}); err != nil || n != 1 {
		t.Fatalf("sent touched %d rows (%v), want 1", n, err)
	}
	if c := claimOf(t, db, row); c.Status != "sent" || c.ClaimedBy == nil || *c.ClaimedBy != "skymail-b-2" {
		t.Fatalf("sent row = %+v", c)
	}
	// A sent row is final: a late failure from its own claim changes nothing.
	if n, err := db.SetMailQueueItemFailed(ctx, SetMailQueueItemFailedParams{
		ID: row, ClaimedBy: *third.ClaimedBy, ClaimedAt: *third.ClaimedAt, Error: &reason,
	}); err != nil || n != 0 {
		t.Fatalf("failed over a sent row touched %d rows (%v)", n, err)
	}
}

// One process can take the same row twice: its first claim's lease ran out
// (a worker stuck in a send) and its own dispatcher took the row again. The
// first claim's late outcome must not land on the second.
func TestALateOutcomeOfTheSameProcessesEarlierClaimDoesNotLand(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	row := queueOf(t, db, 1)[0]

	first := takeOne(t, db, "skymail-a-1")
	expire(t, db, row)
	second := takeOne(t, db, "skymail-a-1")
	if second.ClaimedAt.Equal(*first.ClaimedAt) {
		t.Fatalf("both claims at %v", first.ClaimedAt)
	}

	stale(t, db, first)
	if c := claimOf(t, db, row); c.Status != "processing" || c.ClaimedAt == nil || !c.ClaimedAt.Equal(*second.ClaimedAt) {
		t.Fatalf("row after the first claim's outcomes = %+v, want the second claim's", c)
	}
	if n := attemptsOf(t, db, row); n != 0 {
		t.Fatalf("attempts = %d, want 0", n)
	}
	if n, err := db.SetMailQueueItemSent(ctx, SetMailQueueItemSentParams{
		ID: row, ClaimedBy: *second.ClaimedBy, ClaimedAt: *second.ClaimedAt,
	}); err != nil || n != 1 {
		t.Fatalf("the second claim's sent touched %d rows (%v), want 1", n, err)
	}
}
