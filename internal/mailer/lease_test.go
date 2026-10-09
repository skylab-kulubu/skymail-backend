package mailer_test

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

func TestQueueLeaseFromEnv(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      mailer.DefaultQueueLease,
		"  ":    mailer.DefaultQueueLease,
		"10m":   10 * time.Minute,
		"5m":    5 * time.Minute,
		" 15m ": 15 * time.Minute,
		"1h":    time.Hour,
		"24h":   24 * time.Hour,
	} {
		got, err := mailer.QueueLeaseFromEnv(func(name string) string {
			if name != mailer.QueueLeaseEnv {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Errorf("%q = %v (%v), want %v", raw, got, err, want)
		}
	}
	if mailer.DefaultQueueLease != 10*time.Minute {
		t.Errorf("default lease = %v, want 10m", mailer.DefaultQueueLease)
	}
	for _, raw := range []string{"10", "ten minutes", "1m", "4m59s", "0", "-5m", "24h1s", "100000h"} {
		if got, err := mailer.QueueLeaseFromEnv(func(string) string { return raw }); err == nil {
			t.Errorf("%q = %v, want an error that stops startup", raw, got)
		}
	}
}

// A lease has to outlast a send by a margin, or a row whose send is still in
// progress could be taken and sent by another process.
func TestEveryAllowedLeaseOutlastsASend(t *testing.T) {
	if mailer.MinQueueLease < 3*mailer.SendBudget {
		t.Fatalf("shortest lease %v is under three sends of %v", mailer.MinQueueLease, mailer.SendBudget)
	}
	if mailer.DefaultQueueLease < mailer.MinQueueLease || mailer.DefaultQueueLease > mailer.MaxQueueLease {
		t.Fatalf("default lease %v outside [%v, %v]", mailer.DefaultQueueLease, mailer.MinQueueLease, mailer.MaxQueueLease)
	}
	// The reset takes the lease as int4 seconds.
	if mailer.MaxQueueLease.Seconds() > math.MaxInt32 {
		t.Fatalf("longest lease %v does not fit the reset's seconds", mailer.MaxQueueLease)
	}
}

// silentRelay accepts connections and never says a word: a relay stuck before
// its greeting. It holds each connection until the test ends.
func silentRelay(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener
}

// The deadline the dialler sets is the end of the send: a later, longer
// deadline from go-mail, or none, does not move it.
func TestABudgetedConnectionEndsAtItsBudget(t *testing.T) {
	relay := silentRelay(t)
	conn, err := mailer.DialWithin(300*time.Millisecond)(context.Background(), "tcp", relay.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = conn.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("read from a silent relay = %v, want a timeout", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("read gave up after %v, want about the 300ms budget", took)
	}
}
