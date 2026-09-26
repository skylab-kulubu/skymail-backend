package mailer

import (
	"fmt"
	"strings"
)

// Sender is whether this process sends the mail it queues, from MAIL_SENDER.
//
// Paused is how SkyMail starts after a restore from backup: the queue the dump
// brought back can hold mail to people erased since it was taken, and nothing
// may go out until core has replayed its erasures (account erasure spec §8).
// Mail is still queued and the API answers as ever; only the dispatcher and
// the workers do not start. It is an environment value and not a row, because
// a restored dump would bring a row's old value back with it.
type Sender string

const (
	SenderOn     Sender = "on"
	SenderPaused Sender = "paused"
)

// SenderFromEnv reads MAIL_SENDER: on, or unset, sends; paused does not. Any
// other value is an error that stops startup, so a typo cannot quietly send
// the queue a restore brought back.
func SenderFromEnv(getenv func(string) string) (Sender, error) {
	raw := strings.TrimSpace(getenv("MAIL_SENDER"))
	switch Sender(raw) {
	case "", SenderOn:
		return SenderOn, nil
	case SenderPaused:
		return SenderPaused, nil
	default:
		return "", fmt.Errorf("MAIL_SENDER must be on or paused, not %q", raw)
	}
}
