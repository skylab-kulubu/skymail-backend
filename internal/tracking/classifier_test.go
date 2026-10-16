package tracking

import (
	"testing"
	"time"
)

func TestClassifierRules(t *testing.T) {
	sent := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		ua, method, kind, class string
		seconds                 int
		opened, automatic       bool
	}{
		{"Mozilla/5.0", "GET", "open", "apple_proxy_likely", 120, false, true},
		{"GoogleImageProxy", "GET", "open", "google_proxy", 31, false, false},
		{"GoogleImageProxy", "GET", "open", "google_proxy", 30, false, true},
		{"YahooMailProxy", "GET", "open", "yahoo_proxy", 30, false, true},
		{"Microsoft Outlook/16.0", "GET", "open", "microsoft", 120, false, false},
		{"Mozilla/5.0 Chrome/120.0 Safari/537.36", "GET", "open", "browser", 120, false, false},
		{"Thunderbird/120", "GET", "open", "mail_client", 120, false, false},
		{"curl/8.0", "GET", "open", "scanner_likely", 120, false, true},
		{"unrecognized", "GET", "open", "scanner_likely", 120, false, true},
		{"", "GET", "open", "scanner_likely", 120, false, true},
		{"Mozilla/5.0 Chrome/120", "HEAD", "open", "scanner_likely", 120, false, true},
		{"Mozilla/5.0 Chrome/120", "GET", "open", "browser", 10, false, true},
		{"Mozilla/5.0 Chrome/120", "GET", "open", "browser", 11, false, false},
		{"Mozilla/5.0 Chrome/120", "GET", "click", "browser", 60, false, true},
		{"Mozilla/5.0 Chrome/120", "GET", "click", "browser", 61, false, false},
		{"Mozilla/5.0 Chrome/120", "GET", "click", "browser", 60, true, false},
	} {
		got := Classify(tc.ua, tc.method, tc.kind, sent, sent.Add(time.Duration(tc.seconds)*time.Second), tc.opened)
		if got.SourceClass != tc.class || got.AutomatedLikely != tc.automatic || got.Version != ClassifierVersion {
			t.Errorf("%+v: %+v", tc, got)
		}
	}
}
