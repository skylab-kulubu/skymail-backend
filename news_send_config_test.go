package main

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/news"
)

// SKYMAIL_NEWS_SEND is off unless on; on needs the CMS's address and
// CORE_URL; any other value stops startup, naming the variable.
func TestNewsSendConfigFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	lanes := mailer.SendLanes{ListUnsubscribe: true, Purpose: true}
	cfg, err := newsSendConfigFromEnv(env(nil), "", mailer.SendLanes{})
	if err != nil || cfg.Options.Enabled || cfg.Options.ReadMore != news.ReadMoreHome || len(cfg.Options.ImageHosts) != 1 || cfg.Options.ImageHosts[0] != "yildizskylab.com" {
		t.Fatalf("unset: %+v %v", cfg, err)
	}
	cfg, err = newsSendConfigFromEnv(env(map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080/",
		"SKYMAIL_NEWS_READ_MORE": "pages", "SKYMAIL_NEWS_IMAGE_HOSTS": " API.yildizskylab.com , cdn.example.org"}), "http://core-backend:8080", lanes)
	if err != nil || !cfg.Options.Enabled || cfg.CMSURL != "http://inscribed:8080" || cfg.Options.ReadMore != news.ReadMorePages ||
		len(cfg.Options.ImageHosts) != 2 || cfg.Options.ImageHosts[0] != "api.yildizskylab.com" {
		t.Fatalf("on: %+v %v", cfg, err)
	}
	for name, test := range map[string]struct {
		values map[string]string
		core   string
		lanes  mailer.SendLanes
		names  string
	}{
		"bad switch":         {map[string]string{"SKYMAIL_NEWS_SEND": "yes"}, "", lanes, "SKYMAIL_NEWS_SEND"},
		"bad read more":      {map[string]string{"SKYMAIL_NEWS_READ_MORE": "app"}, "", lanes, "SKYMAIL_NEWS_READ_MORE"},
		"bad cms url":        {map[string]string{"SKYMAIL_NEWS_CMS_URL": "inscribed:8080"}, "", lanes, "SKYMAIL_NEWS_CMS_URL"},
		"bad image hosts":    {map[string]string{"SKYMAIL_NEWS_IMAGE_HOSTS": "https://cdn.example.org/"}, "", lanes, "SKYMAIL_NEWS_IMAGE_HOSTS"},
		"on, no cms":         {map[string]string{"SKYMAIL_NEWS_SEND": "on"}, "http://core-backend:8080", lanes, "SKYMAIL_NEWS_CMS_URL"},
		"on, no core":        {map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080"}, "", lanes, "CORE_URL"},
		"on, no unsubscribe": {map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080"}, "http://core-backend:8080", mailer.SendLanes{Purpose: true}, "SKYMAIL_LIST_UNSUBSCRIBE"},
		"on, no purpose":     {map[string]string{"SKYMAIL_NEWS_SEND": "on", "SKYMAIL_NEWS_CMS_URL": "http://inscribed:8080"}, "http://core-backend:8080", mailer.SendLanes{ListUnsubscribe: true}, "SKYMAIL_MAIL_PURPOSE"},
	} {
		if _, err := newsSendConfigFromEnv(env(test.values), test.core, test.lanes); err == nil || !strings.Contains(err.Error(), test.names) {
			t.Errorf("%s: %v, want an error naming %s", name, err, test.names)
		}
	}
}
