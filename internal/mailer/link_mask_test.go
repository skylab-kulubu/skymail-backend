package mailer_test

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// A tracked link's target is shown as it is, unless it carries a one-time
// link's or a code's part: then only its scheme and host.
func TestMaskLinkURLHidesOneTimeLinksAndCodes(t *testing.T) {
	for raw, want := range map[string]string{
		"https://yildizskylab.com/etkinlikler":                                "https://yildizskylab.com/etkinlikler",
		"https://yildizskylab.com/etkinlikler/yaz-kampi?utm_source=duyuru":    "https://yildizskylab.com/etkinlikler/yaz-kampi?utm_source=duyuru",
		"https://forms.yildizskylab.com/f/kayit":                              "https://forms.yildizskylab.com/f/kayit",
		"https://my.yildizskylab.com/verify?token=abc":                        "https://my.yildizskylab.com/" + mailer.Mask,
		"https://my.yildizskylab.com/login?code=482910":                       "https://my.yildizskylab.com/" + mailer.Mask,
		"https://api.yildizskylab.com/v1/public/tickets/Qx7Kp2Lm9Rt4Vw8Zb3Nc": "https://api.yildizskylab.com/" + mailer.Mask,
		"https://skyl.app/QR-88K2":                                            "https://skyl.app/" + mailer.Mask,
		"https://example.org/davet?e=ayse%40example.com":                      "https://example.org/" + mailer.Mask,
		"https://example.org/sayfa#eyJhbGciOiJIUzI1NiJ9":                      "https://example.org/" + mailer.Mask,
		"https://example.org/bilet/12345678":                                  "https://example.org/" + mailer.Mask,
		"not a link":                                                          mailer.Mask,
	} {
		got, masked := mailer.MaskLinkURL(raw)
		if got != want || masked != (got != raw) {
			t.Errorf("MaskLinkURL(%q) = %q, %v; want %q", raw, got, masked, want)
		}
	}
}
