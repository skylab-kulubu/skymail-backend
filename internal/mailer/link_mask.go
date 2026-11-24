package mailer

import (
	"net/url"
	"strings"
)

// MaskLinkURL is a tracked link's target as the tracking reports show it
// (Yusuf, 2026-11-24: "her şeyi görelim"): as it is, unless it carries what
// the masking of sensitive content hides — a one-time link or a code. Then
// it is its scheme and host with the rest masked (https://host/••••••), and
// masked is true. A link carries one when a query parameter is named like a
// secret (token, code, key, password, …) or its value is shaped like one (a
// code, a token, an address, an IP address, four or more digits, 16 or more
// characters), when a path segment is code- or token-shaped or 16 or more
// characters long, or when it has a fragment of eight or more characters. A
// value that is not a link is masked whole.
func MaskLinkURL(raw string) (shown string, masked bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return Mask, true
	}
	if !linkCarriesCredential(parsed) {
		return raw, false
	}
	return parsed.Scheme + "://" + parsed.Host + "/" + Mask, true
}

func linkCarriesCredential(u *url.URL) bool {
	if u.User != nil || len(u.Fragment) >= 8 {
		return true
	}
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		if secretName(name) || personalName(name) || len(value) >= 16 || secretShaped(value) || strings.Contains(value, "@") {
			return true
		}
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		if len(segment) >= 16 || codeShaped(segment) || tokenShaped(segment) || digitRun.MatchString(segment) && len(segment) >= 4 {
			return true
		}
	}
	return false
}
