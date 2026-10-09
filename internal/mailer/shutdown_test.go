package mailer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

func stopWithin(t *testing.T, m mailer.Transactional, budget time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return m.Stop(ctx)
}

// Rows the dispatcher took that no worker has begun go back to pending the
// moment the process stops, unclaimed and with no attempt counted: the next
// task sends them at once instead of after the lease. Stopped, the process
// takes no more.
func TestStopGivesBackTheRowsNoWorkerBegan(t *testing.T) {
	captureMailerLogs(t)
	db, ids := pendingQueue(t, 5)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, func(context.Context, database.MailQueue) error {
		t.Error("a row was sent; no worker runs in this test")
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Three idle slots and no worker to take what the dispatcher puts in
	// the channel: the rows stay taken and unsent, as at a stop that comes
	// between the dispatcher and the workers.
	mailer.StartWithoutWorkers(m, ctx, 3)
	m.Wake()
	waitFor(t, "the dispatcher to take three rows", 10*time.Second, func() bool {
		taken := 0
		for _, row := range leaseRows(t, db) {
			if row.Status == "processing" && row.ClaimedBy == mailer.Claimant(m) {
				taken++
			}
		}
		return taken == 3
	})

	if err := stopWithin(t, m, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	rows := leaseRows(t, db)
	for _, id := range ids {
		if row := rows[id]; row.Status != "pending" || row.ClaimedBy != "" || row.Attempts != 0 {
			t.Errorf("row %s after Stop = %+v, want pending, unclaimed, no attempt", id, row)
		}
	}

	m.Wake()
	time.Sleep(300 * time.Millisecond)
	for id, row := range leaseRows(t, db) {
		if row.Status != "pending" {
			t.Errorf("row %s = %+v after a wake past Stop, want it left pending", id, row)
		}
	}
}

// The sends in progress at a stop finish and record their outcome; Stop
// returns only then. Nothing new is taken meanwhile.
func TestStopLetsTheSendsInProgressFinish(t *testing.T) {
	captureMailerLogs(t)
	db, _ := pendingQueue(t, 5)
	relay := newRelay()
	entered := make(chan struct{}, 5)
	answer := make(chan struct{})
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, func(ctx context.Context, job database.MailQueue) error {
		entered <- struct{}{}
		select {
		case <-answer:
		case <-ctx.Done():
			return ctx.Err()
		}
		return relay.deliverAfter("stopping", 0)(ctx, job)
	})
	startMailer(t, m, 3)
	m.Wake()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("three sends did not start")
		}
	}

	stopped := make(chan error, 1)
	go func() { stopped <- stopWithin(t, m, 10*time.Second) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) with three sends in progress", err)
	case <-time.After(300 * time.Millisecond):
	}
	inProgress, waiting := 0, 0
	for _, row := range leaseRows(t, db) {
		switch {
		case row.Status == "processing" && row.ClaimedBy == mailer.Claimant(m):
			inProgress++
		case row.Status == "pending" && row.ClaimedBy == "":
			waiting++
		}
	}
	if inProgress != 3 || waiting != 2 {
		t.Fatalf("while stopping: %d in progress, %d waiting; want 3 and 2", inProgress, waiting)
	}

	close(answer)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the sends had finished")
	}
	sent, pending := 0, 0
	for _, row := range leaseRows(t, db) {
		switch row.Status {
		case "sent":
			sent++
		case "pending":
			pending++
		}
	}
	if sent != 3 || pending != 2 || relay.count() != 3 {
		t.Fatalf("after Stop: %d sent, %d pending, %d at the relay; want 3, 2, 3", sent, pending, relay.count())
	}

	m.Wake()
	time.Sleep(300 * time.Millisecond)
	if relay.count() != 3 || len(entered) != 0 {
		t.Fatalf("a stopped process sent more: %d at the relay", relay.count())
	}
}

// A send still in progress when Stop's time runs out is left to its lease,
// and cutting it off (Start's context ending) counts no failed attempt: the
// row was not tried and refused, the process went away.
func TestASendCutOffByShutdownCountsNoAttempt(t *testing.T) {
	logs := captureMailerLogs(t)
	db, ids := pendingQueue(t, 1)
	entered := make(chan struct{}, 1)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn)
	mailer.SetDeliver(m, func(ctx context.Context, _ database.MailQueue) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, abandon := context.WithCancel(context.Background())
	t.Cleanup(abandon)
	m.Start(ctx, 1)
	m.Wake()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the send did not start")
	}

	if err := stopWithin(t, m, 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with a send that never ends: %v, want its deadline", err)
	}
	abandon()
	time.Sleep(500 * time.Millisecond)

	row := leaseRows(t, db)[ids[0]]
	if row.Status != "processing" || row.ClaimedBy != mailer.Claimant(m) || row.Attempts != 0 {
		t.Fatalf("row cut off by shutdown = %+v, want still claimed with no attempt counted", row)
	}
	for _, line := range logs.lines(t) {
		if line["message"] == "Retrying email" || line["message"] == "Giving up on email" {
			t.Fatalf("a send cut off by shutdown was logged as a failure: %v", line)
		}
	}
}

// Paused, a process sends nothing; Stop only stops its reaper.
func TestStopAPausedMailer(t *testing.T) {
	captureMailerLogs(t)
	db := restoredQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderPaused)
	startMailer(t, m, 3)
	if err := stopWithin(t, m, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
