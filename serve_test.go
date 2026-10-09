package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/health"
)

type lines struct {
	mu   sync.Mutex
	text []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text = append(l.text, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *lines) has(fragment string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.text {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

// slowApp answers GET /slow once release closes, and tells entered when a
// request is in.
func slowApp(entered chan<- struct{}, release <-chan struct{}) *fiber.App {
	app := fiber.New()
	app.Get("/slow", func(c fiber.Ctx) error {
		entered <- struct{}{}
		<-release
		return c.SendString("finished")
	})
	return app
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// worker is a background loop as shutdown sees it: it runs until its
// context is cancelled, then takes a moment to settle its claim.
func worker(ctx context.Context, settle time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		time.Sleep(settle)
	}()
	return done
}

// On the stop signal: readiness answers 503 at once, the port stops taking
// connections, the request in flight is answered in full (with Connection:
// close), the workers keep running until the requests are done, and serve
// returns only after every worker has stopped.
func TestServeDrainsTheRequestInFlightThenStopsTheWorkers(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	app := slowApp(entered, release)
	ln := listen(t)
	addr := ln.Addr().String()
	signal, stop := context.WithCancel(context.Background())
	defer stop()
	workers, stopWorkers := context.WithCancel(context.Background())
	readiness := health.NewReadiness(nil, health.Options{})
	settled := worker(workers, 50*time.Millisecond)
	var log lines
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, app, ln, shutdownPlan{
			Readiness: readiness, HTTPDrain: 5 * time.Second, Total: 10 * time.Second,
			StopWorkers: stopWorkers, Wait: []stopping{stopping{"test worker", settled}},
			Logf: log.logf,
		})
	}()

	type answer struct {
		status int
		body   string
		close  bool
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		response, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		answered <- answer{status: response.StatusCode, body: string(body), close: response.Close}
	}()
	<-entered

	stop()
	waitFor(t, "readiness to drain", func() bool { return readiness.Draining() })
	waitFor(t, "the port to refuse connections", func() bool {
		conn, err := net.DialTimeout("tcp4", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		return false
	})
	if workers.Err() != nil {
		t.Fatal("the workers were stopped while a request was in flight")
	}
	select {
	case err := <-served:
		t.Fatalf("serve returned with a request in flight: %v", err)
	default:
	}

	close(release)
	got := <-answered
	if got.err != nil || got.status != http.StatusOK || got.body != "finished" {
		t.Fatalf("request in flight: %+v", got)
	}
	if !got.close {
		t.Fatal("the last answer did not say Connection: close")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	select {
	case <-settled:
	default:
		t.Fatal("serve returned before the worker had stopped")
	}
	if workers.Err() == nil {
		t.Fatal("the workers were not stopped")
	}
}

// A request still open at the drain deadline is cut off; the workers are
// stopped all the same and serve returns.
func TestServeCutsOffRequestsAtTheDrainDeadline(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	app := slowApp(entered, release)
	ln := listen(t)
	addr := ln.Addr().String()
	signal, stop := context.WithCancel(context.Background())
	workers, stopWorkers := context.WithCancel(context.Background())
	var log lines
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, app, ln, shutdownPlan{
			HTTPDrain: 100 * time.Millisecond, Total: 5 * time.Second,
			StopWorkers: stopWorkers, Wait: []stopping{stopping{"test worker", worker(workers, 0)}},
			Logf: log.logf,
		})
	}()
	go func() {
		response, err := http.Get("http://" + addr + "/slow")
		if err == nil {
			response.Body.Close()
		}
	}()
	<-entered
	started := time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the drain deadline")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("serve took %s with a 100ms drain", elapsed)
	}
	if workers.Err() == nil {
		t.Fatal("the workers were not stopped")
	}
	if !log.has("cut off") {
		t.Fatalf("no line about the request cut off: %q", log.text)
	}
}

// A worker that does not stop by the shutdown deadline is named in the log
// and left behind: the process exits before Swarm's SIGKILL.
func TestServeGivesUpOnAWorkerAtTheDeadline(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	ln := listen(t)
	signal, stop := context.WithCancel(context.Background())
	never := make(chan struct{})
	var log lines
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, app, ln, shutdownPlan{
			HTTPDrain: time.Second, Total: 200 * time.Millisecond, StopWorkers: func() {},
			Wait: []stopping{stopping{"stuck worker", never}},
			Logf: log.logf,
		})
	}()
	waitFor(t, "the port to answer", func() bool {
		conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve waited past its deadline")
	}
	if !log.has("stuck worker") {
		t.Fatalf("the stuck worker is not named: %q", log.text)
	}
}

// A stop signal before serving (during startup) shuts down without
// serving anything.
func TestServeAfterTheSignalServesNothing(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	signal, stop := context.WithCancel(context.Background())
	stop()
	workers, stopWorkers := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(signal, fiber.New(), ln, shutdownPlan{
			HTTPDrain: time.Second, Total: time.Second, StopWorkers: stopWorkers,
			Wait: []stopping{stopping{"test worker", worker(workers, 0)}}, Logf: func(string, ...any) {},
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	if conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("the port still takes connections")
	}
}

// SIGTERM (what Docker sends on a deploy or scale down) and SIGINT end the
// context serve watches; the process is not killed.
func TestStopSignalsEndTheServeContext(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		ctx, release := stopSignals()
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%v did not end the context", sig)
		}
		release()
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The pools close after the workers have stopped, within the shutdown's
// budget: a close that blocks (pgxpool waits for every connection in use)
// is named in the log and left behind, and serve returns by Total.
func TestServeClosesAfterTheWorkersAndGivesUpAtTheDeadline(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	signal, stop := context.WithCancel(context.Background())
	workers, stopWorkers := context.WithCancel(context.Background())
	settled := worker(workers, 50*time.Millisecond)
	var closedAfterWorkers, otherClosed bool
	var log lines
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, fiber.New(), ln, shutdownPlan{
			HTTPDrain: time.Second, Total: 300 * time.Millisecond, StopWorkers: stopWorkers,
			Wait: []stopping{stopping{"test worker", settled}},
			Close: []closing{
				{name: "redis", close: func() {
					select {
					case <-settled:
						closedAfterWorkers = true
					default:
					}
				}},
				{name: "stuck pool", close: func() { select {} }},
				{name: "after the stuck one", close: func() { otherClosed = true }},
			},
			Logf: log.logf,
		})
	}()
	waitFor(t, "the port to answer", func() bool {
		conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve waited on a close past its deadline")
	}
	if !closedAfterWorkers {
		t.Fatal("a close ran before the workers had stopped")
	}
	if otherClosed {
		t.Fatal("closes ran past the deadline")
	}
	if !log.has("stuck pool") {
		t.Fatalf("the stuck close is not named: %q", log.text)
	}
}

// The mailer starts stopping at the signal, beside the drain (no new rows are
// taken while requests finish), and the background loops are cancelled only
// once it has stopped: its sends in progress finish instead of being cut off.
func TestServeStopsTheMailerAtTheSignalAndTheLoopsAfterIt(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	app := slowApp(entered, release)
	ln := listen(t)
	addr := ln.Addr().String()
	signal, stop := context.WithCancel(context.Background())
	workers, stopWorkers := context.WithCancel(context.Background())
	mailerStarted, mailerRelease := make(chan struct{}), make(chan struct{})
	var cancelledBeforeMailerStopped bool
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, app, ln, shutdownPlan{
			HTTPDrain: 5 * time.Second, Total: 10 * time.Second,
			StopMailer: func(ctx context.Context) error {
				close(mailerStarted)
				<-mailerRelease
				cancelledBeforeMailerStopped = workers.Err() != nil
				return nil
			},
			StopWorkers: stopWorkers, Wait: []stopping{{"test worker", worker(workers, 0)}},
			Logf: func(string, ...any) {},
		})
	}()
	go func() {
		response, err := http.Get("http://" + addr + "/slow")
		if err == nil {
			_, _ = io.ReadAll(response.Body)
			response.Body.Close()
		}
	}()
	<-entered
	stop()
	select {
	case <-mailerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the mailer did not start stopping while a request was in flight")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if workers.Err() != nil {
		t.Fatal("the loops were cancelled before the mailer had stopped")
	}
	close(mailerRelease)
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if cancelledBeforeMailerStopped {
		t.Fatal("the loops were cancelled while the mailer was stopping")
	}
	if workers.Err() == nil {
		t.Fatal("the loops were not cancelled")
	}
}

// A mailer that does not stop by Total does not hold the process past it.
func TestServeGivesUpOnTheMailerAtTheDeadline(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	signal, stop := context.WithCancel(context.Background())
	var log lines
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, fiber.New(), ln, shutdownPlan{
			HTTPDrain: time.Second, Total: 200 * time.Millisecond,
			StopMailer: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			StopWorkers: func() {},
			Logf:        log.logf,
		})
	}()
	waitFor(t, "the port to answer", func() bool {
		conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve waited on the mailer past its deadline")
	}
	if !log.has("mailer did not stop") {
		t.Fatalf("the mailer's late stop is not logged: %q", log.text)
	}
}

// The mailer's stop ends SendCutGrace before Total: then the sends still in
// progress are cut, and serve waits the rest of Total for their workers (a
// send that finished as the cut came writes its outcome) before it closes the
// pools.
func TestServeLeavesTheCutSendsTheEndOfTheBudget(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	signal, stop := context.WithCancel(context.Background())
	workers, stopWorkers := context.WithCancel(context.Background())
	// A worker cut off by the workers' context takes a moment to return.
	sendsStopped := worker(workers, 100*time.Millisecond)
	var mailerDeadline time.Time
	var signalled time.Time
	var closedAfterTheSends bool
	served := make(chan error, 1)
	go func() {
		served <- serve(signal, fiber.New(), ln, shutdownPlan{
			HTTPDrain: time.Second, Total: 2 * time.Second, SendCutGrace: time.Second,
			StopMailer: func(ctx context.Context) error {
				mailerDeadline, _ = ctx.Deadline()
				<-ctx.Done()
				return ctx.Err()
			},
			StopWorkers: stopWorkers,
			Wait:        []stopping{{"the mailer's sends", sendsStopped}},
			Close: []closing{{name: "pool", close: func() {
				select {
				case <-sendsStopped:
					closedAfterTheSends = true
				default:
				}
			}}},
			Logf: func(string, ...any) {},
		})
	}()
	waitFor(t, "the port to answer", func() bool {
		conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	signalled = time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	if left := signalled.Add(2 * time.Second).Sub(mailerDeadline); left < 800*time.Millisecond || left > 1200*time.Millisecond {
		t.Fatalf("the mailer's stop ended %s before Total, want the 1s grace", left)
	}
	if !closedAfterTheSends {
		t.Fatal("the pools closed before the cut sends had returned")
	}
}
