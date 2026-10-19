package tracking

import (
	"fmt"
	"strings"
	"time"
)

// Retention of open and click tracking (hub ticket iletisim-servisi 14c;
// design note 07 §4.7; docs/data-lifecycle.md, open and click tracking).

// EventRetentionDays is how long a person's openings and clicks are kept,
// from their mail going out (mail_queue.sent_at): a year (Yusuf, 2026-10-19;
// the design said 90 days). Past it no event is written or shown, and the
// retention run deletes the events and closes the token. Every window on
// events — the writers', the reports', the retention run's — is this one.
const EventRetentionDays = 365

// EventRetention is EventRetentionDays as a duration.
const EventRetention = EventRetentionDays * 24 * time.Hour

// TotalsMinRecipients is the fewest tracked recipients a send's counts
// without people are kept for once its events go (T4): fewer, and a count
// could still tell about one person.
const TotalsMinRecipients = 10

// RetentionMode is what the retention run does: SKYMAIL_TRACKING_RETENTION.
type RetentionMode string

const (
	// RetentionOff runs nothing. The default.
	RetentionOff RetentionMode = "off"
	// RetentionDryRun runs every rule in a transaction it rolls back: it
	// counts what apply would change and changes nothing.
	RetentionDryRun RetentionMode = "dry-run"
	// RetentionApply changes the rows.
	RetentionApply RetentionMode = "apply"
	// RetentionPurge deletes every opening and click and closes every token,
	// whatever their age: the full roll-back of tracking. Counts without
	// people and the links stay; old mail's links still redirect.
	RetentionPurge RetentionMode = "purge"
)

// RetentionModeEnv names the variable that holds the mode.
const RetentionModeEnv = "SKYMAIL_TRACKING_RETENTION"

// RetentionModeFromEnv reads SKYMAIL_TRACKING_RETENTION: off (or unset),
// dry-run, apply or purge. Anything else stops startup, so a typo never turns
// destruction on or off. purge needs SKYMAIL_TRACKING off: it says there is
// no tracking left, and new mail must not be tracked under it.
func RetentionModeFromEnv(getenv func(string) string, cfg Config) (RetentionMode, error) {
	raw := strings.TrimSpace(getenv(RetentionModeEnv))
	var mode RetentionMode
	switch RetentionMode(raw) {
	case "", RetentionOff:
		mode = RetentionOff
	case RetentionDryRun, RetentionApply, RetentionPurge:
		mode = RetentionMode(raw)
	default:
		return "", fmt.Errorf("%s must be off, dry-run, apply or purge, not %q", RetentionModeEnv, raw)
	}
	if mode == RetentionPurge && cfg.On() {
		return "", fmt.Errorf("%s=purge needs SKYMAIL_TRACKING=off: turn tracking off before deleting all of it", RetentionModeEnv)
	}
	return mode, nil
}
