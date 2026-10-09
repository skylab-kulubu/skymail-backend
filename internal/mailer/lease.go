package mailer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// QueueLeaseEnv names how long a queue row a process has taken stays that
// process's own.
const QueueLeaseEnv = "MAIL_QUEUE_LEASE"

const (
	// SendBudget bounds one SMTP send, from dialling the relay to QUIT: the
	// greeting, EHLO, STARTTLS, AUTH and DATA included. go-mail's own timeouts
	// bound only the TCP dial and the commands after its connection check; a
	// relay that accepts the connection and then stalls in the greeting, the
	// TLS handshake or AUTH would hold a worker without end. The connection's
	// deadline (dialWithin) ends the send instead.
	SendBudget = time.Minute
	// DefaultQueueLease is the lease when MAIL_QUEUE_LEASE is unset. A process
	// takes a row only for an idle worker, so the lease has to cover one send,
	// which SendBudget bounds; a process that died hands its rows on after it.
	DefaultQueueLease = 10 * time.Minute
	// MinQueueLease keeps a lease well past SendBudget: a lease that ran out
	// under a send still in progress would let another process send the row
	// too.
	MinQueueLease = 5 * time.Minute
	// MaxQueueLease keeps a dead process's rows from waiting longer than a
	// day, and the lease within what the reset's int seconds can hold.
	MaxQueueLease = 24 * time.Hour
)

// QueueLeaseFromEnv reads MAIL_QUEUE_LEASE, a Go duration such as 10m. Unset
// is DefaultQueueLease. A value that does not parse, or lies outside
// MinQueueLease and MaxQueueLease, is an error that stops startup.
func QueueLeaseFromEnv(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(QueueLeaseEnv))
	if raw == "" {
		return DefaultQueueLease, nil
	}
	lease, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 10m, not %q", QueueLeaseEnv, raw)
	}
	if lease < MinQueueLease || lease > MaxQueueLease {
		return 0, fmt.Errorf("%s must be between %s and %s, not %s", QueueLeaseEnv, MinQueueLease, MaxQueueLease, lease)
	}
	return lease, nil
}

// Option changes how NewMailer makes the mailer.
type Option func(*mailerImpl)

// WithQueueLease sets the queue lease (MAIL_QUEUE_LEASE). Without it, or with
// a lease that is not positive, the lease is DefaultQueueLease.
func WithQueueLease(lease time.Duration) Option {
	return func(m *mailerImpl) {
		if lease > 0 {
			m.lease = lease
		}
	}
}

// newClaimant names this process in the queue rows it takes: the host name,
// which is the task's container under Swarm, and a random suffix, so a
// restarted container is not taken for the process it replaced.
func newClaimant() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "skymail"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("%s-%d", host, time.Now().UnixNano())
	}
	return host + "-" + hex.EncodeToString(suffix)
}

// dialWithin dials the relay with a deadline budget from now that no later
// deadline can move: go-mail sets its own deadlines during the send, and each
// is held to this one.
func dialWithin(budget time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		end := time.Now().Add(budget)
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		budgeted := &budgetConn{Conn: conn, end: end}
		if err := budgeted.SetDeadline(time.Time{}); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return budgeted, nil
	}
}

// budgetConn is a connection whose deadlines never pass end.
type budgetConn struct {
	net.Conn
	end time.Time
}

func (c *budgetConn) within(t time.Time) time.Time {
	if t.IsZero() || t.After(c.end) {
		return c.end
	}
	return t
}

func (c *budgetConn) SetDeadline(t time.Time) error { return c.Conn.SetDeadline(c.within(t)) }

func (c *budgetConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(c.within(t))
}

func (c *budgetConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.within(t))
}
