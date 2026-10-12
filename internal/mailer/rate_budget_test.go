package mailer_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// The send budget (SKYMAIL_RATE_PER_MINUTE, SKYMAIL_DAILY_RECIPIENT_BUDGET):
// the relay's limits hold across every process, the bulk lane gets at most
// BulkSharePercent of them, and a single mail a person is waiting for is never
// held back or dropped for it.

func TestRateBudgetFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want mailer.RateBudget
	}{
		{nil, mailer.RateBudget{PerMinute: 25, PerDay: 9000}},
		{map[string]string{mailer.RatePerMinuteEnv: " 30 ", mailer.DailyRecipientBudgetEnv: "10000"}, mailer.RateBudget{PerMinute: 30, PerDay: 10000}},
		{map[string]string{mailer.RatePerMinuteEnv: "0"}, mailer.RateBudget{PerMinute: 0, PerDay: 9000}},
		{map[string]string{mailer.RatePerMinuteEnv: "0", mailer.DailyRecipientBudgetEnv: "0"}, mailer.RateBudget{}},
	} {
		got, err := mailer.RateBudgetFromEnv(func(key string) string { return c.env[key] })
		if err != nil || got != c.want {
			t.Errorf("RateBudgetFromEnv(%v) = %+v, %v; want %+v", c.env, got, err, c.want)
		}
	}
	for _, env := range []map[string]string{
		{mailer.RatePerMinuteEnv: "-1"},
		{mailer.RatePerMinuteEnv: "25/m"},
		{mailer.DailyRecipientBudgetEnv: "9.000"},
		{mailer.DailyRecipientBudgetEnv: "1e4"},
	} {
		if got, err := mailer.RateBudgetFromEnv(func(key string) string { return env[key] }); err == nil {
			t.Errorf("RateBudgetFromEnv(%v) = %+v, want an error that stops startup", env, got)
		}
	}
	if !(mailer.RateBudget{}).Off() || (mailer.RateBudget{PerDay: 1}).Off() {
		t.Error("Off: only a budget with neither limit is off")
	}
}

// The room a window leaves the bulk lane: the bulk lane's share less what it
// took, and the whole limit less what both lanes took, the previous window
// weighed by how much of it still lies within a window's span of now.
func TestBulkRoomInAWindow(t *testing.T) {
	for _, c := range []struct {
		name                                     string
		limit                                    int
		elapsed                                  float64
		curBulk, curSystem, prevBulk, prevSystem int
		want                                     int
	}{
		{"an empty window: the bulk share, 70% of 25", 25, 0, 0, 0, 0, 0, 17},
		{"the bulk share taken", 25, 0.5, 17, 0, 0, 0, 0},
		{"system mail used the rest of the limit", 25, 0.5, 5, 20, 0, 0, 0},
		{"system mail past the limit: the bulk lane yields", 25, 0, 0, 40, 0, 0, 0},
		{"system mail within its share leaves the bulk share", 25, 0, 0, 8, 0, 0, 17},
		{"system mail past its share narrows the bulk share", 25, 0, 0, 10, 0, 0, 15},
		{"the previous window full, at the start of this one", 25, 0, 0, 0, 17, 8, 0},
		{"the previous window full, half way through this one", 25, 0.5, 0, 0, 17, 8, 8},
		{"the previous window full, at the end of this one", 25, 0.999, 0, 0, 17, 8, 16},
		{"a limit of 1 still sends a list", 1, 0, 0, 0, 0, 0, 1},
		{"the day: 70% of 9000", 9000, 0.3, 100, 50, 0, 0, 6200},
	} {
		got := mailer.WindowRoom(c.limit, c.elapsed, c.curBulk, c.curSystem, c.prevBulk, c.prevSystem)
		if got != c.want {
			t.Errorf("%s: room %d, want %d", c.name, got, c.want)
		}
	}
}

// testClock is the instant the budget reads instead of the database's clock.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	// The start of a minute, so the minute window's arithmetic is plain.
	return &testClock{at: time.Now().UTC().Truncate(time.Minute)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// budgeted is a mailer with budget on db, reading clock, ready to take rows
// a turn at a time for slots workers.
func budgeted(t *testing.T, db *database.Store, budget mailer.RateBudget, clock *testClock, slots int) mailer.Transactional {
	t.Helper()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithRateBudget(budget))
	mailer.SetClock(m, clock.now)
	mailer.PrepareTurns(m, slots)
	return m
}

// byLane counts rows by lane.
func byLane(rows []database.MailQueue) (system, bulk int) {
	for _, row := range rows {
		if row.Priority > 0 {
			system++
		} else {
			bulk++
		}
	}
	return system, bulk
}

// Two replicas take from one list, turn after turn, at the same time: in one
// minute they take 17 bulk rows between them (70% of 25), not 17 each. Thirty
// single mails queued then all go, though the minute is past its limit, and
// the bulk lane yields to them for as long as they weigh in the window.
func TestTwoReplicasShareTheMinuteBudget(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 100)
	clock := newTestClock()
	budget := mailer.RateBudget{PerMinute: 25}
	replicas := []mailer.Transactional{budgeted(t, db, budget, clock, 10), budgeted(t, db, budget, clock, 10)}
	enqueueList(t, replicas[0])
	ctx := context.Background()

	var bulkTaken [2]atomic.Int32
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		for i, m := range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, bulk := byLane(mailer.Turn(ctx, m))
				bulkTaken[i].Add(int32(bulk))
			}()
		}
		wg.Wait()
	}
	total := bulkTaken[0].Load() + bulkTaken[1].Load()
	if total != 17 {
		t.Fatalf("two replicas took %d + %d bulk rows in one minute, want 17 together", bulkTaken[0].Load(), bulkTaken[1].Load())
	}
	var processing int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE status = 'processing'`).Scan(&processing); err != nil {
		t.Fatal(err)
	}
	if processing != 17 {
		t.Fatalf("%d rows taken in the database, want 17", processing)
	}

	for i := range 30 {
		enqueueSingle(t, replicas[1], fmt.Sprintf("kod%d@example.com", i))
	}
	var system, bulk int
	for range 4 {
		s, b := byLane(mailer.Turn(ctx, replicas[1]))
		system, bulk = system+s, bulk+b
	}
	if system != 30 || bulk != 0 {
		t.Fatalf("with the minute's budget spent, turns took %d single mails and %d bulk rows; want all 30 and none", system, bulk)
	}

	// The next minute: the last one (17 bulk, 30 system) still weighs fully.
	clock.advance(time.Minute)
	if _, bulk := byLane(mailer.Turn(ctx, replicas[0])); bulk != 0 {
		t.Fatalf("at the start of the next minute the bulk lane took %d rows, want none", bulk)
	}
	// A minute later the spent minute lies a whole window back.
	clock.advance(time.Minute)
	bulk = 0
	for range 3 {
		_, b := byLane(mailer.Turn(ctx, replicas[0]))
		bulk += b
	}
	if bulk != 17 {
		t.Fatalf("two minutes on the bulk lane took %d rows, want 17", bulk)
	}
}

// The day's budget holds the bulk lane past any minute: once it has taken its
// share of the day it waits, minute after minute, while single mail still
// goes; two days on it sends again.
func TestDailyBudgetHoldsTheBulkLane(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 30)
	clock := newTestClock()
	m := budgeted(t, db, mailer.RateBudget{PerDay: 10}, clock, 10)
	enqueueList(t, m)
	ctx := context.Background()

	bulk := 0
	for range 3 {
		_, b := byLane(mailer.Turn(ctx, m))
		bulk += b
	}
	if bulk != 7 {
		t.Fatalf("the bulk lane took %d rows of a day's budget of 10, want 7", bulk)
	}

	clock.advance(time.Minute)
	enqueueSingle(t, m, "kod@example.com")
	if system, bulk := byLane(mailer.Turn(ctx, m)); system != 1 || bulk != 0 {
		t.Fatalf("a minute later a turn took %d single mails and %d bulk rows, want 1 and none", system, bulk)
	}

	clock.advance(48 * time.Hour)
	if _, bulk := byLane(mailer.Turn(ctx, m)); bulk != 7 {
		t.Fatalf("two days on the bulk lane took %d rows, want 7", bulk)
	}
}

// Without a budget (both limits 0) the bulk lane takes what its workers can,
// as before the budget, and nothing is counted.
func TestNoBudgetTakesAsBefore(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 50)
	m := budgeted(t, db, mailer.RateBudget{}, newTestClock(), 10)
	enqueueList(t, m)
	enqueueSingle(t, m, "kod@example.com")
	ctx := context.Background()

	bulk := 0
	for range 8 {
		_, b := byLane(mailer.Turn(ctx, m))
		bulk += b
	}
	if bulk != 50 {
		t.Fatalf("without a budget the bulk lane took %d of 50 rows", bulk)
	}
	var windows int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_send_windows`).Scan(&windows); err != nil {
		t.Fatal(err)
	}
	if windows != 0 {
		t.Fatalf("%d send windows counted without a budget, want none", windows)
	}
}

// Two replicas running, a list going out and the minute's bulk budget spent:
// a single mail queued on either reaches the relay at once.
func TestSingleMailGoesWhileTheBulkBudgetIsSpent(t *testing.T) {
	logs := captureMailerLogs(t)
	db := laneQueue(t, 60)
	clock := newTestClock()
	budget := mailer.RateBudget{PerMinute: 25, PerDay: 9000}

	var bulkSent, singleSent atomic.Int32
	deliver := func(_ context.Context, job database.MailQueue) error {
		if job.Priority > 0 {
			singleSent.Add(1)
		} else {
			bulkSent.Add(1)
		}
		return nil
	}
	var replicas []mailer.Transactional
	for range 2 {
		m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithRateBudget(budget))
		mailer.SetClock(m, clock.now)
		mailer.SetDeliver(m, deliver)
		replicas = append(replicas, m)
	}
	enqueueList(t, replicas[0])
	for _, m := range replicas {
		startMailer(t, m, 3)
	}
	waitFor(t, "the minute's bulk budget", 10*time.Second, func() bool { return bulkSent.Load() >= 17 })

	enqueueSingle(t, replicas[1], "kod@example.com")
	waitFor(t, "the single mail", 3*time.Second, func() bool { return singleSent.Load() == 1 })
	if sent := bulkSent.Load(); sent != 17 {
		t.Fatalf("%d bulk rows sent in one minute by two replicas, want 17", sent)
	}
	logged := false
	for _, line := range logs.lines(t) {
		if line["rate_per_minute"] == float64(25) && line["daily_recipient_budget"] == float64(9000) {
			logged = true
		}
	}
	if !logged {
		t.Fatal("startup does not log the send budget")
	}
}
