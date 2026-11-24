package main

import (
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skynotify-backend/internal/unsubscribe"
)

// logUnsubscribeLink says at startup whether SkyNotify's own unsubscribe link
// is on, where it is served and which key version seals new links: never a
// key. On with an empty ring, it warns: /u/ answers 503 and no mail gets a
// SkyNotify link until a key is set (ops/wizards, hub ticket abonelik-cikisi
// 06).
func logUnsubscribeLink(cfg unsubscribe.Config) {
	event := log.Info()
	if cfg.On && cfg.Ring.Empty() {
		event = log.Warn()
	}
	event.Bool("on", cfg.On).Str("base_url", cfg.BaseURL).Str("account_url", cfg.AccountURL).
		Int("key_current", int(cfg.Ring.Current())).Int("key_versions", cfg.Ring.Versions()).
		Msg("one-click unsubscribe link (SKYNOTIFY_UNSUBSCRIBE_LINK, SKYNOTIFY_UNSUBSCRIBE_KEY_CURRENT, SKYNOTIFY_UNSUBSCRIBE_KEY_V<n>)")
}
