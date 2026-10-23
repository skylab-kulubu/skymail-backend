package handlers

import "testing"

// An inbox link stays on the club's ground: an in-app path, or an https
// address on yildizskylab.com or a subdomain whose host the type's text
// writes out (Yusuf, 2026-10-20: Account center on the web). Nothing else, so
// an item is never an open redirect.
func TestAnInboxLinkIsAnInAppPathOrAnAddressOnTheClubsDomain(t *testing.T) {
	for link, want := range map[string]bool{
		"":                                   true,
		"/events/42":                         true,
		"/news/bahar-senligi?from=inbox#top": true,
		"https://yildizskylab.com":           true,
		"https://yildizskylab.com/":          true,
		"https://my.yildizskylab.com/bildirimler": true,
		"https://my.yildizskylab.com?tab=inbox":   true,
		"https://a.b.yildizskylab.com/x#y":        true,

		"//evil.example/x":                           false,
		`/\evil.example`:                             false,
		"events/42":                                  false,
		"http://my.yildizskylab.com/":                false,
		"HTTPS://my.yildizskylab.com/":               false,
		"https://evil.example/":                      false,
		"https://yildizskylab.com.evil.example/":     false,
		"https://evilyildizskylab.com/":              false,
		"https://evil.example?.yildizskylab.com":     false,
		"https://evil.example#.yildizskylab.com":     false,
		"https://yildizskylab.com@evil.example/":     false,
		"https://user@my.yildizskylab.com/":          false,
		"https://my.yildizskylab.com:8443/":          false,
		"https://my..yildizskylab.com/":              false,
		"https://-x.yildizskylab.com/":               false,
		"https://my.yildizskylab.com/a b":            false,
		`https://my.yildizskylab.com\@evil.example/`: false,
		"https:///my.yildizskylab.com":               false,
		"javascript:alert(1)":                        false,
		"mailto:a@yildizskylab.com":                  false,
	} {
		if got := validLink(link); got != want {
			t.Errorf("validLink(%q) = %v, want %v", link, got, want)
		}
	}
}

// In the type's text the host is written out: a variable may fill the path,
// never the host.
func TestAnInboxLinksHostIsWrittenOutInTheText(t *testing.T) {
	for link, want := range map[string]bool{
		"/events/{{.EventID}}":                   true,
		"https://my.yildizskylab.com/x/{{.ID}}":  true,
		"https://my.yildizskylab.com?id={{.ID}}": true,
		"https://{{.Host}}/x":                    false,
		"https://{{.Sub}}.yildizskylab.com/x":    false,
		"https://my.yildizskylab.com{{.Rest}}":   false,
		"{{.Link}}":                              false,
		"https://evil.example/{{.X}}":            false,
	} {
		if got := allowedLinkPrefix(link); got != want {
			t.Errorf("allowedLinkPrefix(%q) = %v, want %v", link, got, want)
		}
	}
}
