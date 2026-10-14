package tracking

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

const (
	// MaxLinkLength is the longest address a click is counted on.
	MaxLinkLength = 2048
	// MaxLinksPerSend is how many of a send's links, the first ones, a click
	// is counted on: a bound on mail_task_links per send.
	MaxLinksPerSend = 100
	// NoTrackAttribute marks an <a> a template does not want counted.
	NoTrackAttribute = "data-skymail-notrack"
)

// Links are the addresses of body's <a> tags a click may be counted on, in
// the order they first appear, each once, as a browser reads them: entities
// decoded, blanks around trimmed. An <a> marked data-skymail-notrack is left
// out, and so is every address Trackable refuses. Nothing outside an <a>
// (stylesheets, images, text, conditional comments) is a link.
func Links(body, baseURL string) []string {
	var links []string
	seen := map[string]bool{}
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return links
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "a" || !hasAttr {
			continue
		}
		href, notrack := anchorAttributes(z)
		if notrack || href == "" || seen[href] || !Trackable(href, baseURL) {
			continue
		}
		seen[href] = true
		links = append(links, href)
	}
}

// anchorAttributes reads the current <a>'s first href, trimmed, and whether
// it is marked data-skymail-notrack.
func anchorAttributes(z *html.Tokenizer) (href string, notrack bool) {
	found := false
	for more := true; more; {
		var key, value []byte
		key, value, more = z.TagAttr()
		switch string(key) {
		case NoTrackAttribute:
			notrack = true
		case "href":
			if !found {
				href, found = strings.TrimSpace(string(value)), true
			}
		}
	}
	return href, notrack
}

// CommonLinks are the links every one of a send's mails has (Links), in the
// order the first mail has them, at most MaxLinksPerSend: a link a variable
// made personal differs between recipients and is never counted.
func CommonLinks(bodies []string, baseURL string) []string {
	if len(bodies) == 0 {
		return nil
	}
	common := Links(bodies[0], baseURL)
	for _, body := range bodies[1:] {
		if len(common) == 0 {
			break
		}
		has := map[string]bool{}
		for _, link := range Links(body, baseURL) {
			has[link] = true
		}
		kept := common[:0]
		for _, link := range common {
			if has[link] {
				kept = append(kept, link)
			}
		}
		common = kept
	}
	if len(common) > MaxLinksPerSend {
		common = common[:MaxLinksPerSend]
	}
	return common
}

// Trackable reports whether a click on raw may be counted: an absolute http
// or https address with a host, no user or password, no blank or control
// character, at most MaxLinkLength long; not an unsubscribe or communication
// preference link (core's /v1/communication-preferences/unsubscribe, the
// account center's /communication-preferences), and not one of SkyMail's own
// /t/ addresses. The address a click redirects to is always one that passed
// here.
func Trackable(raw, baseURL string) bool {
	if raw == "" || len(raw) > MaxLinkLength {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return false
	}
	path := strings.ToLower(u.EscapedPath())
	if strings.Contains(path, "communication-preferences") || strings.Contains(path, "unsubscribe") {
		return false
	}
	if baseURL != "" && strings.HasPrefix(strings.ToLower(raw), strings.ToLower(baseURL)+"/t/") {
		return false
	}
	return true
}
