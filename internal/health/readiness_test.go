package health_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/health"
)

type fakeDatabase struct {
	mu    sync.Mutex
	err   error
	block bool
	pings int
}

func (d *fakeDatabase) Ping(ctx context.Context) error {
	d.mu.Lock()
	d.pings++
	err, block := d.err, d.block
	d.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (d *fakeDatabase) set(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *fakeDatabase) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pings
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newClock() *clock                   { return &clock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)} }
func discard(string, ...any)             {}
func readiness(db health.Database, c *clock) *health.Readiness {
	return health.NewReadiness(db, health.Options{Now: c.Now, Logf: discard})
}

// The database going away and coming back moves readiness both ways, and
// draining takes the task out for good whatever the database says.
func TestReadinessFollowsTheDatabaseAndThenDraining(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	c := newClock()
	r := readiness(db, c)
	ctx := context.Background()

	if err := r.Check(ctx); err != nil {
		t.Fatalf("database up: %v", err)
	}
	db.set(errors.New("connection refused"))
	c.advance(health.DefaultMaxAge)
	if err := r.Check(ctx); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("database down: %v, want a database error", err)
	}
	db.set(nil)
	c.advance(health.DefaultMaxAge)
	if err := r.Check(ctx); err != nil {
		t.Fatalf("database back: %v", err)
	}
	r.Drain()
	if !r.Draining() {
		t.Fatal("Draining() = false after Drain")
	}
	pings := db.count()
	if err := r.Check(ctx); !errors.Is(err, health.ErrDraining) {
		t.Fatalf("draining: %v, want ErrDraining", err)
	}
	if db.count() != pings {
		t.Fatal("a draining task still pinged the database")
	}
}

// /ready is public: however often it is asked, the database is pinged at
// most once per DefaultMaxAge, and the answer in between is the last one.
func TestReadinessPingsAtMostOncePerMaxAge(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{err: errors.New("down")}
	c := newClock()
	r := readiness(db, c)
	for range 50 {
		if err := r.Check(context.Background()); err == nil {
			t.Fatal("a cached failure answered ready")
		}
	}
	if got := db.count(); got != 1 {
		t.Fatalf("pings=%d for 50 checks within the max age, want 1", got)
	}
	c.advance(health.DefaultMaxAge)
	db.set(nil)
	if err := r.Check(context.Background()); err != nil {
		t.Fatalf("after the max age: %v", err)
	}
	if got := db.count(); got != 2 {
		t.Fatalf("pings=%d, want 2", got)
	}
}

// A database that does not answer is not ready within the ping timeout,
// not whenever the caller gives up.
func TestReadinessGivesUpOnADatabaseThatHangs(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{block: true}
	r := health.NewReadiness(db, health.Options{PingTimeout: 50 * time.Millisecond, Logf: discard})
	started := time.Now()
	err := r.Check(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want a deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Check took %s with a 50ms ping timeout", elapsed)
	}
}

// A caller that hangs up does not leave a failure behind for the next one.
func TestReadinessIgnoresTheCallersCancellation(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	r := readiness(db, newClock())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Check(ctx); err != nil {
		t.Fatalf("cancelled caller: %v", err)
	}
}

// The log says when the database stops answering and when it is back, once
// each, not on every probe.
func TestReadinessLogsTransitionsOnly(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{}
	c := newClock()
	var mu sync.Mutex
	var lines []string
	r := health.NewReadiness(db, health.Options{Now: c.Now, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, format)
	}})
	check := func() {
		_ = r.Check(context.Background())
		c.advance(health.DefaultMaxAge)
	}
	check()
	db.set(errors.New("down"))
	check()
	check()
	check()
	db.set(nil)
	check()
	check()
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("log lines=%q, want one when it failed and one when it came back", lines)
	}
}

// Without a database (tests, tools) readiness answers from draining alone.
func TestReadinessWithoutADatabase(t *testing.T) {
	t.Parallel()
	r := health.NewReadiness(nil, health.Options{})
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if err := r.Check(context.Background()); !errors.Is(err, health.ErrDraining) {
		t.Fatalf("err=%v, want ErrDraining", err)
	}
	var none *health.Readiness
	if err := none.Check(context.Background()); err != nil {
		t.Fatalf("nil readiness: %v", err)
	}
}

// slowDatabase answers each ping after delay, counting them.
type slowDatabase struct {
	delay time.Duration
	mu    sync.Mutex
	pings int
}

func (d *slowDatabase) Ping(ctx context.Context) error {
	d.mu.Lock()
	d.pings++
	d.mu.Unlock()
	select {
	case <-time.After(d.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A database slow under load (1.2 s a ping, over the 1 s max age) and five
// callers of /ready arriving during one ping: they share that ping, and
// none waits longer than the health check's own 3 s. (Each ping in turn,
// as before, took 5 pings and kept the last caller 6 s.)
func TestReadinessSharesOnePingAmongTheCallersWhileItRuns(t *testing.T) {
	t.Parallel()
	db := &slowDatabase{delay: 1200 * time.Millisecond}
	r := health.NewReadiness(db, health.Options{Logf: discard})
	var wg sync.WaitGroup
	waits := make([]time.Duration, 5)
	errs := make([]error, 5)
	for i := range waits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 200 * time.Millisecond)
			started := time.Now()
			errs[i] = r.Check(context.Background())
			waits[i] = time.Since(started)
		}()
	}
	wg.Wait()
	for i := range waits {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if waits[i] >= 3*time.Second {
			t.Fatalf("caller %d waited %s", i, waits[i])
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.pings != 1 {
		t.Fatalf("pings=%d for callers arriving during one ping, want 1", db.pings)
	}
}

// The answer's age counts from when the ping answered, not from when it
// began: a ping longer than the max age is still reused by the next caller.
func TestReadinessAgesTheAnswerFromWhenThePingEnded(t *testing.T) {
	t.Parallel()
	c := newClock()
	db := &advancingDatabase{clock: c, by: 1500 * time.Millisecond}
	r := health.NewReadiness(db, health.Options{Now: c.Now, Logf: discard})
	for range 2 {
		if err := r.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if db.pings != 1 {
		t.Fatalf("pings=%d, want the second caller to reuse the first answer", db.pings)
	}
}

// advancingDatabase moves the test clock by `by` during each ping.
type advancingDatabase struct {
	clock *clock
	by    time.Duration
	pings int
}

func (d *advancingDatabase) Ping(context.Context) error {
	d.pings++
	d.clock.advance(d.by)
	return nil
}

// A caller that joins a ping waits at most the ping timeout.
func TestReadinessBoundsTheWaitOfACallerThatJoinsAPing(t *testing.T) {
	t.Parallel()
	db := &fakeDatabase{block: true}
	r := health.NewReadiness(db, health.Options{PingTimeout: 200 * time.Millisecond, Logf: discard})
	go func() { _ = r.Check(context.Background()) }()
	for db.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("a joined ping that timed out answered ready")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the joining caller waited %s with a 200ms ping timeout", elapsed)
	}
}
