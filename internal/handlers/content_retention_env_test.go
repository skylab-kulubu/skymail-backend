package handlers

import "testing"

func TestContentRetentionDaysFromEnv(t *testing.T) {
	for raw, want := range map[string]int{"": 365, "  ": 365, "90": 90, " 30 ": 30, "1": 1, "3650": 3650} {
		got, err := ContentRetentionDaysFromEnv(func(name string) string {
			if name != ContentRetentionEnv {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Errorf("%q = %d (%v), want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "-1", "3651", "90d", "2160h", "ninety", "off"} {
		if got, err := ContentRetentionDaysFromEnv(func(string) string { return raw }); err == nil {
			t.Errorf("%q = %d, want an error that stops startup", raw, got)
		}
	}
}

// Who opened a masked mail is kept two years by default (Yusuf, 2026-10-14).
func TestViewRetentionDaysFromEnv(t *testing.T) {
	for raw, want := range map[string]int{"": 730, "  ": 730, "365": 365, " 30 ": 30, "1": 1, "3650": 3650} {
		got, err := ViewRetentionDaysFromEnv(func(name string) string {
			if name != ViewRetentionEnv {
				t.Fatalf("read %s", name)
			}
			return raw
		})
		if err != nil || got != want {
			t.Errorf("%q = %d (%v), want %d", raw, got, err, want)
		}
	}
	if ViewRetentionEnv != "SENT_CONTENT_VIEWS_RETENTION_DAYS" {
		t.Errorf("ViewRetentionEnv = %s", ViewRetentionEnv)
	}
	for _, raw := range []string{"0", "-1", "3651", "730d", "2y", "off"} {
		if got, err := ViewRetentionDaysFromEnv(func(string) string { return raw }); err == nil {
			t.Errorf("%q = %d, want an error that stops startup", raw, got)
		}
	}
}
