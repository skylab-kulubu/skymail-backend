package main

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// This build has no /t/ routes (ticket 14b): SKYMAIL_TRACKING=on would send
// mail whose pixel and links lead nowhere, so startup refuses it. Off, and
// unset, start as before; a malformed value still names its variable.
func TestThisBuildRefusesToStartWithTrackingOn(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	on := map[string]string{
		"SKYMAIL_TRACKING":          "on",
		"SKYMAIL_TRACKING_BASE_URL": "https://api.yildizskylab.com/api/skymail",
		"CORE_URL":                  "http://core-backend:8080",
	}
	if _, err := trackingConfigFromEnv(env(on)); err == nil || !strings.Contains(err.Error(), "14b") ||
		!strings.Contains(err.Error(), "SKYMAIL_TRACKING") {
		t.Fatalf("SKYMAIL_TRACKING=on: %v, want startup refused until the /t/ routes (ticket 14b)", err)
	}
	for _, values := range []map[string]string{{}, {"SKYMAIL_TRACKING": "off"}} {
		cfg, err := trackingConfigFromEnv(env(values))
		if err != nil || cfg.Mode != tracking.ModeOff {
			t.Errorf("%v: %+v, %v; want off", values, cfg, err)
		}
	}
	if _, err := trackingConfigFromEnv(env(map[string]string{"SKYMAIL_TRACKING": "evet"})); err == nil ||
		!strings.Contains(err.Error(), "SKYMAIL_TRACKING") {
		t.Errorf("a malformed mode: %v", err)
	}
}
