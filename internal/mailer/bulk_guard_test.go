package mailer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// The bulk lane's guards (duyuru-haber 05, G3 and G4): a throttling reply of
// the relay holds every process's bulk lane and costs a bulk row no attempt;
// a send whose recipients fail past the threshold pauses itself; a paused
// send waits and a cancelled one's pending rows are skipped.

func TestBulkGuardFromEnv(t *testing.T) {
	defaults := mailer.BulkGuard{ThrottlePause: 5 * time.Minute, FailureCount: 10, FailurePercent: 10}
	for _, c := range []struct {
		env  map[string]string
		want mailer.BulkGuard
	}{
		{nil, defaults},
		{map[string]string{mailer.ThrottlePauseEnv: "90s"}, mailer.BulkGuard{ThrottlePause: 90 * time.Second, FailureCount: 10, FailurePercent: 10}},
		{map[string]string{mailer.ThrottlePauseEnv: "0"}, mailer.BulkGuard{FailureCount: 10, FailurePercent: 10}},
		{map[string]string{mailer.FailurePauseCountEnv: "0", mailer.FailurePausePercentEnv: "100"}, mailer.BulkGuard{ThrottlePause: 5 * time.Minute, FailurePercent: 100}},
	} {
		got, err := mailer.BulkGuardFromEnv(func(key string) string { return c.env[key] })
		if err != nil || got != c.want {
			t.Errorf("BulkGuardFromEnv(%v) = %+v, %v; want %+v", c.env, got, err, c.want)
		}
	}
	for _, env := range []map[string]string{
		{mailer.ThrottlePauseEnv: "5"},
		{mailer.ThrottlePauseEnv: "-1m"},
		{mailer.ThrottlePauseEnv: "2h"},
		{mailer.FailurePauseCountEnv: "-1"},
		{mailer.FailurePausePercentEnv: "0"},
		{mailer.FailurePausePercentEnv: "101"},
		{mailer.FailurePausePercentEnv: "%10"},
	} {
		if got, err := mailer.BulkGuardFromEnv(func(key string) string { return env[key] }); err == nil {
			t.Errorf("BulkGuardFromEnv(%v) = %+v, want an error that stops startup", env, got)
		}
	}
}

func TestWhichRepliesAreThrottling(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{mailer.SMTPReply("MAIL FROM", 421, "4.7.0"), true},
		{mailer.SMTPReply("DATA", 421, ""), true},
		{mailer.SMTPReply("RCPT TO", 432, "4.7.12"), true},
		{mailer.SMTPReply("DATA", 451, "4.7.500"), true},
		{mailer.SMTPReply("RCPT TO", 450, "4.2.1"), false},
		{mailer.SMTPReply("RCPT TO", 451, "4.4.0"), false},
		{mailer.SMTPReply("MAIL FROM", 554, "5.2.0"), false},
		{mailer.SMTPReply("RCPT TO", 550, "5.7.1"), false},
		{mailer.SMTPReply("RCPT TO", 550, "5.1.1"), false},
		{mailer.GraphReply(429, time.Minute), true},
		{mailer.GraphReply(503, 0), true},
		{mailer.GraphReply(500, 0), false},
		{mailer.GraphReply(403, 0), false},
		{errors.New("send failed: dial: 421 4.7.0 Too many connections"), true},
		{errors.New("send failed: connection reset by peer"), false},
		{errors.New("render: 421 is in the subject"), false},
	} {
		if got := mailer.Throttled(c.err); got != c.want {
			t.Errorf("Throttled(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

// guarded is a mailer with guard and budget on db, reading clock, ready to
// take rows a turn at a time for slots workers, sending with deliver.
func guarded(t *testing.T, db *database.Store, guard mailer.BulkGuard, budget mailer.RateBudget, clock *testClock, slots int, deliver func(context.Context, database.MailQueue) error) mailer.Transactional {
	t.Helper()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithRateBudget(budget), mailer.WithBulkGuard(guard))
	mailer.SetClock(m, clock.now)
	mailer.SetDeliver(m, deliver)
	mailer.PrepareTurns(m, slots)
	return m
}

type guardRow struct {
	Status   string
	Attempts int
	Error    string
}

func guardRowOf(t *testing.T, db *database.Store, id uuid.UUID) guardRow {
	t.Helper()
	var row guardRow
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT status::text, attempts, COALESCE(error, '') FROM mail_queue WHERE id = $1`, id,
	).Scan(&row.Status, &row.Attempts, &row.Error); err != nil {
		t.Fatal(err)
	}
	return row
}

func bulkOf(rows []database.MailQueue) []database.MailQueue {
	var bulk []database.MailQueue
	for _, row := range rows {
		if row.Priority <= 0 {
			bulk = append(bulk, row)
		}
	}
	return bulk
}

// An SMTP 421 to a list's mail: the row goes back to pending with no attempt
// counted, and no process takes a bulk row for SKYMAIL_THROTTLE_PAUSE, while a
// login code queued then goes at once. Past the pause the list goes on.
func TestAThrottlingReplyHoldsTheBulkLane(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 10)
	clock := newTestClock()
	ctx := context.Background()
	throttle := func(context.Context, database.MailQueue) error { return mailer.SMTPReply("MAIL FROM", 421, "4.7.0") }
	m := guarded(t, db, mailer.BulkGuard{ThrottlePause: 5 * time.Minute}, mailer.RateBudget{}, clock, 3, throttle)
	replica := guarded(t, db, mailer.BulkGuard{ThrottlePause: 5 * time.Minute}, mailer.RateBudget{}, clock, 3, throttle)
	enqueueList(t, m)

	taken := bulkOf(mailer.Turn(ctx, m))
	if len(taken) == 0 {
		t.Fatal("the first turn took no bulk row")
	}
	if err := mailer.SendRow(ctx, m, taken[0]); err != nil {
		t.Fatal(err)
	}
	row := guardRowOf(t, db, taken[0].ID)
	if row.Status != "pending" || row.Attempts != 0 || !strings.Contains(row.Error, "421") {
		t.Fatalf("the throttled row is %+v, want pending with no attempt counted and the reply as its error", row)
	}

	enqueueSingle(t, replica, "kod@example.com")
	if system, bulk := byLane(mailer.Turn(ctx, replica)); system != 1 || bulk != 0 {
		t.Fatalf("after the 421 another replica's turn took %d single mails and %d bulk rows, want 1 and none", system, bulk)
	}
	clock.advance(4 * time.Minute)
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk != 0 {
		t.Fatalf("4 minutes after the 421 the bulk lane took %d rows, want none", bulk)
	}
	clock.advance(2 * time.Minute)
	if _, bulk := byLane(mailer.Turn(ctx, replica)); bulk == 0 {
		t.Fatal("6 minutes after the 421 the bulk lane still takes nothing")
	}
}

// A single mail the relay throttles keeps its retries as before (an attempt
// counted), and still holds the bulk lane: the account is the one throttled.
func TestAThrottledSingleMailCountsItsAttemptAndHoldsTheBulkLane(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 5)
	clock := newTestClock()
	ctx := context.Background()
	m := guarded(t, db, mailer.BulkGuard{ThrottlePause: 5 * time.Minute}, mailer.RateBudget{PerMinute: 25, PerDay: 9000}, clock, 3,
		func(context.Context, database.MailQueue) error { return mailer.SMTPReply("DATA", 451, "4.7.500") })
	single := enqueueSingle(t, m, "kod@example.com")
	taken := mailer.Turn(ctx, m)
	var job database.MailQueue
	for _, row := range taken {
		if row.TaskID == single {
			job = row
		}
	}
	if job.ID == uuid.Nil {
		t.Fatal("the turn did not take the single mail")
	}
	if err := mailer.SendRow(ctx, m, job); err != nil {
		t.Fatal(err)
	}
	if row := guardRowOf(t, db, job.ID); row.Status != "pending" || row.Attempts != 1 {
		t.Fatalf("the throttled single mail is %+v, want pending with its attempt counted", row)
	}
	enqueueList(t, m)
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk != 0 {
		t.Fatalf("after a throttled single mail the bulk lane took %d rows, want none", bulk)
	}
}

// Graph's 429 asks for longer than SKYMAIL_THROTTLE_PAUSE: the hold lasts as
// long as Graph asked.
func TestGraphsRetryAfterLengthensTheHold(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 10)
	clock := newTestClock()
	ctx := context.Background()
	m := guarded(t, db, mailer.BulkGuard{ThrottlePause: 5 * time.Minute}, mailer.RateBudget{PerMinute: 25, PerDay: 9000}, clock, 3,
		func(context.Context, database.MailQueue) error { return mailer.GraphReply(429, 10*time.Minute) })
	enqueueList(t, m)
	taken := bulkOf(mailer.Turn(ctx, m))
	if len(taken) == 0 {
		t.Fatal("the first turn took no bulk row")
	}
	if err := mailer.SendRow(ctx, m, taken[0]); err != nil {
		t.Fatal(err)
	}
	if row := guardRowOf(t, db, taken[0].ID); row.Status != "pending" || row.Attempts != 0 {
		t.Fatalf("the row Graph throttled is %+v, want pending with no attempt counted", row)
	}
	clock.advance(6 * time.Minute)
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk != 0 {
		t.Fatalf("6 minutes after a 429 asking for 10 the bulk lane took %d rows, want none", bulk)
	}
	clock.advance(5 * time.Minute)
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk == 0 {
		t.Fatal("11 minutes after a 429 asking for 10 the bulk lane still takes nothing")
	}
}

// With the hold off (SKYMAIL_THROTTLE_PAUSE=0, or no guard) a 421 is retried
// as before: an attempt counted, nothing held.
func TestWithoutTheHoldA421IsAnAttempt(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 10)
	clock := newTestClock()
	ctx := context.Background()
	m := guarded(t, db, mailer.BulkGuard{}, mailer.RateBudget{}, clock, 3,
		func(context.Context, database.MailQueue) error { return mailer.SMTPReply("MAIL FROM", 421, "4.7.0") })
	enqueueList(t, m)
	taken := bulkOf(mailer.Turn(ctx, m))
	if err := mailer.SendRow(ctx, m, taken[0]); err != nil {
		t.Fatal(err)
	}
	if row := guardRowOf(t, db, taken[0].ID); row.Attempts != 1 {
		t.Fatalf("without the hold the 421 row is %+v, want an attempt counted", row)
	}
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk == 0 {
		t.Fatal("without the hold the bulk lane stopped after a 421")
	}
}

func taskPause(t *testing.T, db *database.Store, task uuid.UUID) (pausedReason string, sendFailures int) {
	t.Helper()
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT COALESCE(paused_reason, ''), send_failures FROM mail_tasks WHERE id = $1`, task,
	).Scan(&pausedReason, &sendFailures); err != nil {
		t.Fatal(err)
	}
	return pausedReason, sendFailures
}

// A send whose recipients fail for good pauses itself once 3 failed and they
// are at least half of those the relay answered: its other rows wait, another
// send goes on. Resumed, it is taken again and counts afresh.
func TestASendPausesItselfPastItsFailureRate(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 10)
	clock := newTestClock()
	ctx := context.Background()
	var failing uuid.UUID
	deliver := func(_ context.Context, job database.MailQueue) error {
		if job.TaskID == failing {
			return mailer.SMTPReply("RCPT TO", 550, "5.1.1")
		}
		return nil
	}
	m := guarded(t, db, mailer.BulkGuard{FailureCount: 3, FailurePercent: 50}, mailer.RateBudget{}, clock, 10, deliver)
	failing = enqueueList(t, m)

	taken := bulkOf(mailer.Turn(ctx, m))
	if len(taken) < 4 {
		t.Fatalf("the first turn took %d bulk rows, want at least 4", len(taken))
	}
	for i, job := range taken[:3] {
		if err := mailer.SendRow(ctx, m, job); err != nil {
			t.Fatal(err)
		}
		reason, failures := taskPause(t, db, failing)
		if failures != i+1 {
			t.Fatalf("after %d failures the send counts %d", i+1, failures)
		}
		if paused := reason == "failure_rate"; paused != (i == 2) {
			t.Fatalf("after %d failures paused_reason is %q", i+1, reason)
		}
	}
	other := enqueueList(t, m)
	next := bulkOf(mailer.Turn(ctx, m))
	if len(next) == 0 {
		t.Fatal("with one send paused the other send's rows were not taken")
	}
	for _, job := range next {
		if job.TaskID != other {
			t.Fatalf("a turn took a row of the paused send")
		}
	}

	if n, err := db.ResumeMailTask(ctx, failing); err != nil || n != 1 {
		t.Fatalf("ResumeMailTask = %d, %v", n, err)
	}
	if _, failures := taskPause(t, db, failing); failures != 0 {
		t.Fatalf("a resumed send still counts %d failures, want 0", failures)
	}
	resumed := false
	for range 3 {
		for _, job := range bulkOf(mailer.Turn(ctx, m)) {
			if job.TaskID == failing {
				resumed = true
			}
		}
	}
	if !resumed {
		t.Fatal("the resumed send's rows are not taken")
	}
}

// Three failures after four sent are 3 of 7: under half, the send goes on.
func TestFailuresUnderThePercentDoNotPause(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 10)
	clock := newTestClock()
	ctx := context.Background()
	sent := 0
	deliver := func(context.Context, database.MailQueue) error {
		if sent < 4 {
			sent++
			return nil
		}
		return mailer.SMTPReply("RCPT TO", 550, "5.1.1")
	}
	m := guarded(t, db, mailer.BulkGuard{FailureCount: 3, FailurePercent: 50}, mailer.RateBudget{}, clock, 10, deliver)
	task := enqueueList(t, m)
	taken := bulkOf(mailer.Turn(ctx, m))
	if len(taken) < 7 {
		t.Fatalf("the first turn took %d bulk rows, want 7", len(taken))
	}
	for _, job := range taken[:7] {
		if err := mailer.SendRow(ctx, m, job); err != nil {
			t.Fatal(err)
		}
	}
	if reason, failures := taskPause(t, db, task); reason != "" || failures != 3 {
		t.Fatalf("after 4 sent and 3 failed the send is paused_reason %q with %d failures, want not paused with 3", reason, failures)
	}
}

// An operator pauses one send: the bulk lane takes only the other's rows.
// Cancelled, the paused send's pending rows are skipped ('cancelled'), a row
// being sent finishes, and a row a retry puts back after the cancel is swept
// by the reaper. Its status never reads failed for the cancel.
func TestPauseAndCancelTouchTheRightRows(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 5)
	clock := newTestClock()
	ctx := context.Background()
	var transient uuid.UUID
	deliver := func(_ context.Context, job database.MailQueue) error {
		if job.ID == transient {
			return errors.New("connection reset by peer")
		}
		return nil
	}
	m := guarded(t, db, mailer.BulkGuard{ThrottlePause: 5 * time.Minute, FailureCount: 10, FailurePercent: 10}, mailer.RateBudget{}, clock, 2, deliver)
	a := enqueueList(t, m)

	// One of A's rows is sent, another is being sent (bulk slots for 2
	// workers: 1).
	first := bulkOf(mailer.Turn(ctx, m))
	if len(first) != 1 {
		t.Fatalf("the first turn took %d rows of A, want 1", len(first))
	}
	if err := mailer.SendRow(ctx, m, first[0]); err != nil {
		t.Fatal(err)
	}
	inFlight := bulkOf(mailer.Turn(ctx, m))
	if len(inFlight) != 1 || inFlight[0].TaskID != a {
		t.Fatalf("the second turn took %d rows of A, want 1", len(inFlight))
	}
	if n, err := db.PauseMailTask(ctx, database.PauseMailTaskParams{ID: a, PausedBy: laneSender}); err != nil || n != 1 {
		t.Fatalf("PauseMailTask = %d, %v", n, err)
	}
	var parked int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status = 'pending' AND next_attempt_at >= '2999-01-01'`, a).Scan(&parked); err != nil || parked != 3 {
		t.Fatalf("the pause parked %d of A's 3 pending rows, %v", parked, err)
	}
	// A row put back due after the pause (a retry, an expired lease) is
	// taken by the query, and handed back parked rather than sent.
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET next_attempt_at = now() - interval '1 minute'
		WHERE id = (SELECT id FROM mail_queue WHERE task_id = $1 AND status = 'pending' LIMIT 1)`, a); err != nil {
		t.Fatal(err)
	}
	b := enqueueList(t, m)
	for range 3 {
		for _, job := range bulkOf(mailer.Turn(ctx, m)) {
			if job.TaskID != b {
				t.Fatal("a turn took a row of the paused send")
			}
		}
	}
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status = 'pending' AND next_attempt_at >= '2999-01-01'`, a).Scan(&parked); err != nil || parked != 3 {
		t.Fatalf("after the turns %d of A's 3 pending rows are parked, %v", parked, err)
	}

	if n, err := db.CancelMailTask(ctx, database.CancelMailTaskParams{ID: a, CancelledBy: laneSender}); err != nil || n != 1 {
		t.Fatalf("CancelMailTask = %d, %v", n, err)
	}
	if n, err := db.CancelPendingRows(ctx, &a); err != nil || n != 3 {
		t.Fatalf("CancelPendingRows = %d, %v; want A's 3 pending rows", n, err)
	}
	if row := guardRowOf(t, db, inFlight[0].ID); row.Status != "processing" {
		t.Fatalf("the row being sent at the cancel is %+v, want still processing", row)
	}
	var bPending int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE task_id = $1 AND status <> 'failed'`, b).Scan(&bPending); err != nil || bPending != 5 {
		t.Fatalf("the cancel touched the other send: %d of its 5 rows left, %v", bPending, err)
	}

	// The row being sent fails for a moment and is put back to pending: a
	// take hands it back written cancelled, never sending it.
	transient = inFlight[0].ID
	if err := mailer.SendRow(ctx, m, inFlight[0]); err != nil {
		t.Fatal(err)
	}
	if row := guardRowOf(t, db, transient); row.Status != "pending" {
		t.Fatalf("the retried row is %+v, want pending", row)
	}
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET next_attempt_at = now() - interval '1 minute' WHERE id = $1`, transient); err != nil {
		t.Fatal(err)
	}
	for _, job := range mailer.Turn(ctx, m) {
		if job.ID == transient {
			t.Fatal("a take handed the cancelled send's retried row to a worker")
		}
	}
	if row := guardRowOf(t, db, transient); row.Status != "failed" || row.Error != "cancelled" {
		t.Fatalf("after the take the retried row is %+v, want failed with 'cancelled'", row)
	}
	// One not yet due is swept by the reaper.
	if _, err := db.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'pending', error = NULL, next_attempt_at = now() + interval '1 hour' WHERE id = $1`, transient); err != nil {
		t.Fatal(err)
	}
	mailer.SweepCancelled(ctx, m)
	if row := guardRowOf(t, db, transient); row.Status != "failed" || row.Error != "cancelled" {
		t.Fatalf("after the sweep the row is %+v, want failed with 'cancelled'", row)
	}
	var status string
	if err := db.Conn.QueryRow(ctx, `SELECT mail_task_status($1)`, a).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("a send cancelled after one recipient was sent reads %s, want sent: the cancelled rows are skipped, not failed", status)
	}
	if n, err := db.ResumeMailTask(ctx, a); err != nil || n != 0 {
		t.Fatalf("ResumeMailTask on a cancelled send = %d, %v; want 0", n, err)
	}
}
