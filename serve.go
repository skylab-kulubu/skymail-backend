package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/health"
)

// The shutdown budget. Docker sends SIGTERM, waits the service's
// stop_grace_period (10 s unless set) and then sends SIGKILL, so
// shutdownTimeout must stay under it with room to spare: run SkyMail with
// stop_grace_period 30 s (docs/health-and-shutdown.md).
const (
	// httpDrainTimeout is how long requests in flight get to finish. A
	// request still open after it is cut off.
	httpDrainTimeout = 20 * time.Second
	// shutdownTimeout is the whole shutdown, from the signal until serve
	// returns and the process exits: the mailer's stop and the drain (side
	// by side), then the background loops and the pools.
	shutdownTimeout = 25 * time.Second
	// sendCutGrace is the end of shutdownTimeout kept for the sends the
	// shutdown cuts off: the mailer's stop gets until 22 s, then the sends
	// still in progress are cut (their connections closed) and their
	// workers have the rest to return, a send that finished at that moment
	// to write its outcome.
	sendCutGrace = 3 * time.Second
)

// stopSignals is the context serve watches: it ends on SIGTERM (Docker's
// stop) or SIGINT (Ctrl-C). Calling release restores the default, so a
// second signal kills at once.
func stopSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
}

// stopping is something shutdown waits on: a background loop. Its channel
// closes when it has stopped.
type stopping struct {
	name string
	done <-chan struct{}
}

// closing is something shutdown closes once everything has stopped: the
// access gate's Redis client, a database pool.
type closing struct {
	name  string
	close func()
}

// shutdownPlan is what serve does once its context ends.
type shutdownPlan struct {
	// Readiness answers 503 from the first moment, so nothing takes this
	// task for one that can serve. Nil skips it.
	Readiness *health.Readiness
	// HTTPDrain bounds the wait for requests in flight; Total bounds the
	// whole shutdown.
	HTTPDrain time.Duration
	Total     time.Duration
	// StopMailer runs from the signal, beside the drain, with a context
	// that ends SendCutGrace before Total: the mailer takes no more rows,
	// gives back the ones no worker began, and lets the sends in progress
	// finish. Nil skips it.
	StopMailer   func(context.Context) error
	SendCutGrace time.Duration
	// StopWorkers cancels the background loops' context once the requests
	// have drained and the mailer has stopped (or its time ran out): the
	// approval expiry sweep, and a send still in progress, whose connection
	// is closed and whose row is left to its lease.
	StopWorkers context.CancelFunc
	// Wait are waited on after StopWorkers, until Total runs out.
	Wait []stopping
	// Close are closed in order after Wait, until Total runs out.
	Close []closing
	Logf  func(format string, args ...any)
}

// serve answers HTTP on ln until ctx ends, then shuts down in order:
// readiness says 503 and the mailer starts stopping; the port stops taking
// connections, idle keep-alive connections are closed and each request in
// flight is answered with Connection: close, for up to HTTPDrain; serve waits
// for the mailer's stop; the background loops are stopped and waited on;
// then what is in Close is closed, in order (the pools last). Whatever has
// not stopped or closed by Total is named in the log and left behind, and
// serve returns: the caller exits at once, before Docker's SIGKILL. Swarm
// takes the task out of the service's virtual IP and waits two seconds
// before it sends SIGTERM (docs/health-and-shutdown.md); a proxy that
// tracks the tasks itself may still open a connection after the port is
// closed, which is refused. It returns an error only when serving fails.
func serve(ctx context.Context, app *fiber.App, ln net.Listener, plan shutdownPlan) error {
	logf := plan.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// fasthttp otherwise answers the last request of a keep-alive
	// connection without saying it closes, and the proxy may send the next
	// request into a closing socket.
	app.Server().CloseOnShutdown = true
	served := make(chan error, 1)
	if ctx.Err() == nil {
		go func() { served <- app.Listener(ln) }()
		select {
		case err := <-served:
			return err
		case <-ctx.Done():
		}
	} else {
		served <- nil
	}

	started := time.Now()
	deadline := started.Add(plan.Total)
	plan.Readiness.Drain()
	mailerStopped := make(chan struct{})
	go func() {
		defer close(mailerStopped)
		if plan.StopMailer == nil {
			return
		}
		stopCtx, cancel := context.WithDeadline(context.Background(), deadline.Add(-plan.SendCutGrace))
		defer cancel()
		if err := plan.StopMailer(stopCtx); err != nil {
			logf("shutdown: the mailer did not stop by the deadline (%v); the sends still in progress are cut off and sent again after their lease", err)
		}
	}()
	logf("shutdown: stop signal; not ready, taking no new connections or queue rows, waiting up to %s for requests in flight", plan.HTTPDrain)
	if err := app.ShutdownWithTimeout(plan.HTTPDrain); err != nil && !errors.Is(err, fiber.ErrNotRunning) {
		logf("shutdown: requests still open after %s were cut off: %v", plan.HTTPDrain, err)
	}
	// Serving may not have begun when the signal came; a closed listener
	// makes it return at once all the same.
	_ = ln.Close()
	<-served

	// StopMailer returns by the deadline.
	<-mailerStopped
	if plan.StopWorkers != nil {
		plan.StopWorkers()
	}
	waitStopped(plan.Wait, deadline, logf)
	closeBy(plan.Close, deadline, logf)
	logf("shutdown: done in %s", time.Since(started).Round(time.Millisecond))
	return nil
}

// waitStopped waits for each of list until deadline, naming in the log the
// ones that had not stopped by then.
func waitStopped(list []stopping, deadline time.Time, logf func(string, ...any)) {
	wait, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, s := range list {
		select {
		case <-s.done:
		case <-wait.Done():
			logf("shutdown: %s did not stop by the deadline", s.name)
		}
	}
}

// closeBy closes each of list in order until deadline. A close still running
// then (pgxpool's Close waits for every connection in use) is named in the
// log and left behind with the ones after it.
func closeBy(list []closing, deadline time.Time, logf func(string, ...any)) {
	wait, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, c := range list {
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			c.close()
		}()
		select {
		case <-closed:
		case <-wait.Done():
			logf("shutdown: closing %s did not finish by the deadline; exiting without it", c.name)
			return
		}
	}
}
