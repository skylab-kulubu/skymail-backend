package tracking_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// Unset, empty or off is off; nothing else is needed then.
func TestConfigFromEnvIsOffByDefault(t *testing.T) {
	for _, raw := range []string{"", " ", "off", " off "} {
		cfg, err := tracking.ConfigFromEnv(env(map[string]string{"SKYMAIL_TRACKING": raw}))
		if err != nil || cfg.Mode != tracking.ModeOff || cfg.On() {
			t.Errorf("SKYMAIL_TRACKING=%q: %+v, %v; want off", raw, cfg, err)
		}
	}
}

// Any other value stops startup, as MAIL_SENDER does: a typo never turns
// tracking on, nor quietly off.
func TestConfigFromEnvRefusesAnyOtherMode(t *testing.T) {
	for _, raw := range []string{"On", "ON", "true", "1", "yes", "enforce", "of"} {
		_, err := tracking.ConfigFromEnv(env(map[string]string{
			"SKYMAIL_TRACKING":          raw,
			"SKYMAIL_TRACKING_BASE_URL": "https://api.yildizskylab.com/api/skymail",
			"CORE_URL":                  "http://core:8080",
		}))
		if err == nil || !strings.Contains(err.Error(), "SKYMAIL_TRACKING") {
			t.Errorf("SKYMAIL_TRACKING=%q: %v, want an error naming the variable", raw, err)
		}
	}
}

// On needs where the mail's /t/ addresses point and where core is; the base
// loses a trailing slash.
func TestConfigFromEnvOnNeedsTheBaseAndCore(t *testing.T) {
	cfg, err := tracking.ConfigFromEnv(env(map[string]string{
		"SKYMAIL_TRACKING":          "on",
		"SKYMAIL_TRACKING_BASE_URL": " https://api.yildizskylab.com/api/skymail/ ",
		"CORE_URL":                  "http://core-backend:8080/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.On() || cfg.BaseURL != "https://api.yildizskylab.com/api/skymail" || cfg.CoreURL != "http://core-backend:8080" {
		t.Fatalf("config = %+v", cfg)
	}

	for name, values := range map[string]map[string]string{
		"SKYMAIL_TRACKING_BASE_URL": {"SKYMAIL_TRACKING": "on", "CORE_URL": "http://core:8080"},
		"CORE_URL":                  {"SKYMAIL_TRACKING": "on", "SKYMAIL_TRACKING_BASE_URL": "https://api.yildizskylab.com/api/skymail"},
	} {
		if _, err := tracking.ConfigFromEnv(env(values)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("on without %s: %v, want an error naming it", name, err)
		}
	}
}

// The base is public and in every tracked mail: https, absolute, nothing a
// mail client or a proxy could take for something else. core is reached over
// the Docker network: http or https. Neither carries credentials, and an
// error names the variable, never the value.
func TestConfigFromEnvChecksTheAddresses(t *testing.T) {
	for _, base := range []string{
		"http://api.yildizskylab.com/api/skymail",
		"api.yildizskylab.com/api/skymail",
		"https://user:secret@api.yildizskylab.com/api/skymail",
		"https://api.yildizskylab.com/api/skymail?x=1",
		"https://api.yildizskylab.com/api/skymail#t",
		"https:///api/skymail",
		"ftp://api.yildizskylab.com",
	} {
		_, err := tracking.ConfigFromEnv(env(map[string]string{
			"SKYMAIL_TRACKING": "on", "SKYMAIL_TRACKING_BASE_URL": base, "CORE_URL": "http://core:8080",
		}))
		if err == nil || !strings.Contains(err.Error(), "SKYMAIL_TRACKING_BASE_URL") || strings.Contains(err.Error(), "secret") {
			t.Errorf("SKYMAIL_TRACKING_BASE_URL=%q: %v, want an error naming the variable and not the value", base, err)
		}
	}
	for _, core := range []string{
		"core:8080",
		"http://user:secret@core:8080",
		"http://core:8080?x=1",
		"http://core:8080#x",
		"tcp://core:8080",
	} {
		_, err := tracking.ConfigFromEnv(env(map[string]string{
			"SKYMAIL_TRACKING": "on", "SKYMAIL_TRACKING_BASE_URL": "https://api.yildizskylab.com/api/skymail", "CORE_URL": core,
		}))
		if err == nil || !strings.Contains(err.Error(), "CORE_URL") || strings.Contains(err.Error(), "secret") {
			t.Errorf("CORE_URL=%q: %v, want an error naming the variable and not the value", core, err)
		}
	}
	// Off, a bad value set beside it still stops startup: it would bite the
	// day tracking is turned on.
	if _, err := tracking.ConfigFromEnv(env(map[string]string{"SKYMAIL_TRACKING_BASE_URL": "http://x"})); err == nil {
		t.Error("off with a bad SKYMAIL_TRACKING_BASE_URL: no error")
	}
}
