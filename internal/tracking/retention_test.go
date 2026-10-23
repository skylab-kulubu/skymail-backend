package tracking_test

import (
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// KVKK notice s6, 2026-10-23: a person's openings and clicks are kept 90
// days from their mail going out, no longer the year of 2026-10-19.
func TestEventRetentionIsNinetyDays(t *testing.T) {
	if tracking.EventRetentionDays != 90 || tracking.EventRetention != 90*24*time.Hour {
		t.Fatalf("event retention = %d days (%s), want 90", tracking.EventRetentionDays, tracking.EventRetention)
	}
	if tracking.TotalsMinRecipients != 10 {
		t.Fatalf("totals threshold = %d, want 10 (T4)", tracking.TotalsMinRecipients)
	}
}

// Unset or off runs nothing; dry-run, apply and purge are the other modes.
// Anything else stops startup, so a typo never turns destruction on or off.
func TestRetentionModeFromEnv(t *testing.T) {
	off := tracking.Config{Mode: tracking.ModeOff}
	for raw, want := range map[string]tracking.RetentionMode{
		"":          tracking.RetentionOff,
		" off ":     tracking.RetentionOff,
		"dry-run":   tracking.RetentionDryRun,
		" apply":    tracking.RetentionApply,
		"purge":     tracking.RetentionPurge,
		"dry-run  ": tracking.RetentionDryRun,
	} {
		got, err := tracking.RetentionModeFromEnv(env(map[string]string{tracking.RetentionModeEnv: raw}), off)
		if err != nil || got != want {
			t.Errorf("%s=%q: %q, %v; want %q", tracking.RetentionModeEnv, raw, got, err, want)
		}
	}
	for _, raw := range []string{"on", "true", "dryrun", "dry_run", "Apply", "delete", "1"} {
		_, err := tracking.RetentionModeFromEnv(env(map[string]string{tracking.RetentionModeEnv: raw}), off)
		if err == nil || !strings.Contains(err.Error(), tracking.RetentionModeEnv) {
			t.Errorf("%s=%q: %v, want an error naming the variable", tracking.RetentionModeEnv, raw, err)
		}
	}
}

// purge deletes every opening and click and closes every token: the full
// roll-back. It needs tracking off, or new mail would be tracked again while
// the purge says there is none.
func TestRetentionPurgeNeedsTrackingOff(t *testing.T) {
	on := tracking.Config{Mode: tracking.ModeOn, BaseURL: "https://api.yildizskylab.com/api/skymail", CoreURL: "http://core:8080"}
	if _, err := tracking.RetentionModeFromEnv(env(map[string]string{tracking.RetentionModeEnv: "purge"}), on); err == nil ||
		!strings.Contains(err.Error(), "SKYMAIL_TRACKING") {
		t.Fatalf("purge with tracking on: %v, want an error naming SKYMAIL_TRACKING", err)
	}
	for _, raw := range []string{"off", "dry-run", "apply"} {
		if _, err := tracking.RetentionModeFromEnv(env(map[string]string{tracking.RetentionModeEnv: raw}), on); err != nil {
			t.Errorf("%s with tracking on: %v", raw, err)
		}
	}
}

// SKYMAIL_TRACKING_RETENTION_MAX_ROWS sets the brake: unset is 50000, a
// whole number from 1 to 10000000 otherwise; anything else stops startup.
func TestRetentionMaxRowsFromEnv(t *testing.T) {
	for raw, want := range map[string]int64{"": 50000, " ": 50000, "1": 1, " 250000 ": 250000, "10000000": 10000000} {
		got, err := tracking.RetentionMaxRowsFromEnv(env(map[string]string{tracking.RetentionMaxRowsEnv: raw}))
		if err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "-5", "10000001", "5e4", "50 000", "abc"} {
		if _, err := tracking.RetentionMaxRowsFromEnv(env(map[string]string{tracking.RetentionMaxRowsEnv: raw})); err == nil ||
			!strings.Contains(err.Error(), tracking.RetentionMaxRowsEnv) {
			t.Errorf("%q: %v, want an error naming the variable", raw, err)
		}
	}
}
