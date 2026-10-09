package mailer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// QueueLeaseEnv names how long a queue row a process has taken stays that
// process's own.
const QueueLeaseEnv = "MAIL_QUEUE_LEASE"

const (
	// DefaultQueueLease is the lease when MAIL_QUEUE_LEASE is unset. A process
	// takes a row only for an idle worker, so the lease has to cover one SMTP
	// send, which go-mail's timeouts keep to well under a minute; ten minutes
	// is ample, and a process that died hands its rows on no later than that.
	DefaultQueueLease = 10 * time.Minute
	// MinQueueLease keeps a lease from running out under a slow send: past the
	// lease, another process may send the row again.
	MinQueueLease = time.Minute
)

// QueueLeaseFromEnv reads MAIL_QUEUE_LEASE, a Go duration such as 10m. Unset
// is DefaultQueueLease. A value that does not parse or is shorter than
// MinQueueLease is an error that stops startup.
func QueueLeaseFromEnv(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(QueueLeaseEnv))
	if raw == "" {
		return DefaultQueueLease, nil
	}
	lease, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 10m, not %q", QueueLeaseEnv, raw)
	}
	if lease < MinQueueLease {
		return 0, fmt.Errorf("%s must be at least %s, not %s", QueueLeaseEnv, MinQueueLease, lease)
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
