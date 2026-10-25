package main

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/news"
)

// SKYMAIL_NEWS_SEND is off unless on; on needs the CMS's address and
// CORE_URL; any other value stops startup, naming the variable.
func TestNewsSendConfigFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cfg, err := newsSendConfigFromEnv(env(nil), "")
	if err != nil || cfg.Options.Enabled || cfg.Options.ReadMore != news.ReadMoreHome {
		t.Fatalf("unset: %+v %v", cfg, err)
	}
	cfg, err = newsSendConfigFromEnv(env(map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080/",
		"SKYMAIL_NEWS_READ_MORE": "pages"}), "http://core-backend:8080")
	if err != nil || !cfg.Options.Enabled || cfg.CMSURL != "http://inscribed:8080" || cfg.Options.ReadMore != news.ReadMorePages {
		t.Fatalf("on: %+v %v", cfg, err)
	}
	for name, test := range map[string]struct {
		values map[string]string
		core   string
		names  string
	}{
		"bad switch":    {map[string]string{"SKYMAIL_NEWS_SEND": "yes"}, "", "SKYMAIL_NEWS_SEND"},
		"bad read more": {map[string]string{"SKYMAIL_NEWS_READ_MORE": "app"}, "", "SKYMAIL_NEWS_READ_MORE"},
		"bad cms url":   {map[string]string{"SKYMAIL_NEWS_CMS_URL": "inscribed:8080"}, "", "SKYMAIL_NEWS_CMS_URL"},
		"on, no cms":    {map[string]string{"SKYMAIL_NEWS_SEND": "on"}, "http://core-backend:8080", "SKYMAIL_NEWS_CMS_URL"},
		"on, no core":   {map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080"}, "", "CORE_URL"},
	} {
		if _, err := newsSendConfigFromEnv(env(test.values), test.core); err == nil || !strings.Contains(err.Error(), test.names) {
			t.Errorf("%s: %v, want an error naming %s", name, err, test.names)
		}
	}
}
