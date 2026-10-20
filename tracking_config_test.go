package main

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Routes are available: valid on starts, off/unset stay off, and malformed
// configuration still stops startup.
func TestTrackingStartupKeepsEnvironmentValidation(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	on := map[string]string{
		"SKYMAIL_TRACKING":          "on",
		"SKYMAIL_TRACKING_BASE_URL": "https://api.yildizskylab.com/api/skymail",
		"CORE_URL":                  "http://core-backend:8080",
	}
	if cfg, err := trackingConfigFromEnv(env(on)); err != nil || !cfg.On() {
		t.Fatalf("valid on configuration: %+v, %v", cfg, err)
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
