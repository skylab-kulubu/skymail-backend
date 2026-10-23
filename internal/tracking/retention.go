package tracking

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention of open and click tracking (hub ticket iletisim-servisi 14c;
// design note 07 §4.7; docs/data-lifecycle.md, open and click tracking).

// EventRetentionDays is how long a person's openings and clicks are kept,
// from their mail going out (mail_queue.sent_at): 90 days (KVKK notice s6,
// 2026-10-23; it replaces Yusuf's 2026-10-19 year). Past it no event is
// written or shown, and the retention run keeps the send's counts without
// people, deletes the events and closes the token: what is left names no
// one and cannot be traced back. Every window on events — the writers', the
// reports', the retention run's — is this one.
const EventRetentionDays = 90

// EventRetention is EventRetentionDays as a duration.
const EventRetention = EventRetentionDays * 24 * time.Hour

// TotalsMinRecipients is the fewest tracked recipients whose counts without
// people are kept for good and shown to anyone who reads the send (T4, #97
// T6): fewer, and a count could still tell about one person. A send with
// fewer keeps its counts too, shown only to who may read its people, and
// the retention run deletes them a year after the send was created
// (mail_tasks.created_at), as its content (KVKK notice s6).
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

// RetentionMaxRowsEnv names the brake: the most tokens, events or links
// one apply may change before it changes nothing (refused_large).
const RetentionMaxRowsEnv = "SKYMAIL_TRACKING_RETENTION_MAX_ROWS"

// DefaultRetentionMaxRows is the brake when RetentionMaxRowsEnv is unset.
const DefaultRetentionMaxRows = 50000

// maxRetentionMaxRows bounds the brake: a typo with extra zeros must not
// turn it off.
const maxRetentionMaxRows = 10000000

// RetentionMaxRowsFromEnv reads SKYMAIL_TRACKING_RETENTION_MAX_ROWS, a whole
// number from 1 to 10,000,000; unset is DefaultRetentionMaxRows. Raising it
// for one release clears a backlog the brake refused. Anything else stops
// startup.
func RetentionMaxRowsFromEnv(getenv func(string) string) (int64, error) {
	raw := strings.TrimSpace(getenv(RetentionMaxRowsEnv))
	if raw == "" {
		return DefaultRetentionMaxRows, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > maxRetentionMaxRows {
		return 0, fmt.Errorf("%s must be a whole number from 1 to %d, not %q", RetentionMaxRowsEnv, maxRetentionMaxRows, raw)
	}
	return n, nil
}
