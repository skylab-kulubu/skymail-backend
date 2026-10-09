package mailer_test

import (
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

func TestQueueLeaseFromEnv(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      mailer.DefaultQueueLease,
		"  ":    mailer.DefaultQueueLease,
		"10m":   10 * time.Minute,
		"1m":    time.Minute,
		" 15m ": 15 * time.Minute,
		"1h":    time.Hour,
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
	for _, raw := range []string{"10", "ten minutes", "59s", "0", "-5m"} {
		if got, err := mailer.QueueLeaseFromEnv(func(string) string { return raw }); err == nil {
			t.Errorf("%q = %v, want an error that stops startup", raw, got)
		}
	}
}
