package mailer_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

func TestSenderFromEnvDefaultsToOn(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{"unset": "", "blank": "  ", "on": "on"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sender, err := mailer.SenderFromEnv(func(key string) string {
				if key == "MAIL_SENDER" {
					return value
				}
				return ""
			})
			if err != nil || sender != mailer.SenderOn {
				t.Fatalf("sender = %q, %v; want on", sender, err)
			}
		})
	}
}

func TestSenderFromEnvReadsPaused(t *testing.T) {
	t.Parallel()

	sender, err := mailer.SenderFromEnv(func(key string) string {
		if key == "MAIL_SENDER" {
			return " paused\n"
		}
		return ""
	})
	if err != nil || sender != mailer.SenderPaused {
		t.Fatalf("sender = %q, %v; want paused", sender, err)
	}
}

// A value that is neither stops startup: a typo must not quietly send the
// queue a restore brought back.
func TestSenderFromEnvRejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"off", "pause", "PAUSED", "Paused", "true", "false", "0", "1", "no"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, err := mailer.SenderFromEnv(func(key string) string {
				if key == "MAIL_SENDER" {
					return value
				}
				return ""
			})
			if err == nil {
				t.Fatalf("MAIL_SENDER=%q accepted", value)
			}
			if !strings.Contains(err.Error(), "MAIL_SENDER must be on or paused") {
				t.Fatalf("error = %q, want it to say what MAIL_SENDER takes", err)
			}
		})
	}
}
