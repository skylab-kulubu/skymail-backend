package news

import (
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The two ways a record is mailed (SPEC §5.7 madde 5).
const (
	// FormatFull: the whole body in the mail.
	FormatFull = "full"
	// FormatSummary: the summary and a link to read on.
	FormatSummary = "summary"
)

// The places "Devamını oku" opens (SKYMAIL_NEWS_READ_MORE).
const (
	// ReadMoreHome: the site's home page, until the site has its News pages
	// (bilet 11); never sky-app.
	ReadMoreHome = "home"
	// ReadMorePages: the record's own page on the site,
	// /haberler/<slug> or /duyurular/<slug>.
	ReadMorePages = "pages"
)

// SiteURL is the club's site.
const SiteURL = "https://yildizskylab.com"

// TemplateKey is the System template a News send is made with (skymail-
// frontend emails/club-news.tsx, written by the Template seed).
const TemplateKey = "club.news"

var (
	markup         = regexp.MustCompile(`<[a-zA-Z/!]`)
	blankLineSplit = regexp.MustCompile(`\n\s*\n`)
)

// BodyHTML is a record's body as HTML: as it is when it holds markup (the
// schema's second version, SPEC §5.3), else the plain text of a record from
// before it turned into paragraphs, as bilet 01's conversion writes them: a
// blank line parts paragraphs, a line break is <br>, &, < and > escaped. The
// mailer narrows it to its allowlist when it renders (safeHTML).
func BodyHTML(body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if markup.MatchString(body) {
		return body
	}
	var out strings.Builder
	for _, paragraph := range blankLineSplit.Split(body, -1) {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		lines := strings.Split(paragraph, "\n")
		for i, line := range lines {
			lines[i] = html.EscapeString(strings.TrimSpace(line))
		}
		out.WriteString("<p>" + strings.Join(lines, "<br>") + "</p>")
	}
	return out.String()
}

// KindLabel is how the mail names the record's kind.
func KindLabel(kind string) string {
	if kind == KindAnnouncement {
		return "Duyuru"
	}
	return "Haber"
}

// Subject is the mail's subject: the one asked for, else the record's
// title, a duyuru's after "Duyuru: " (SPEC §5.7 madde 6).
func Subject(record Record, asked string) string {
	if asked = strings.TrimSpace(asked); asked != "" {
		return asked
	}
	if record.Kind == KindAnnouncement {
		return "Duyuru: " + record.Title
	}
	return record.Title
}

// ReadMoreURL is where the mail's "Devamını oku" goes.
func ReadMoreURL(mode string, record Record) string {
	if mode != ReadMorePages {
		return SiteURL + "/"
	}
	section := "haberler"
	if record.Kind == KindAnnouncement {
		section = "duyurular"
	}
	return SiteURL + "/" + section + "/" + url.PathEscape(record.Slug)
}

// webLink is link when it is an absolute http or https address (mailto too
// with mailto), else empty: a record's image or button never carries
// javascript: or a relative path into a mailbox.
func webLink(link string, mailto bool) string {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	switch {
	case (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil:
		return parsed.String()
	case mailto && parsed.Scheme == "mailto" && parsed.Opaque != "":
		return parsed.String()
	}
	return ""
}

// Variables are the club.news template's variables for a send of record
// (SPEC §5.7 madde 3): the same for every recipient. Each recipient's own
// AudienceReason, CategoryLabel and UnsubscribeUrl come from core.
func Variables(record Record, format, subject, preheader, readMore string) map[string]any {
	full := format == FormatFull
	if strings.TrimSpace(preheader) == "" {
		preheader = record.Summary
	}
	vars := map[string]any{
		"Subject":     Subject(record, subject),
		"Preheader":   strings.TrimSpace(preheader),
		"Title":       record.Title,
		"Summary":     record.Summary,
		"BodyHtml":    "",
		"FullBody":    full,
		"HeroUrl":     webLink(record.HeroImage, false),
		"HeroAlt":     record.HeroAlt,
		"CtaHref":     "",
		"CtaLabel":    "",
		"ReadMoreUrl": ReadMoreURL(readMore, record),
		"KindLabel":   KindLabel(record.Kind),
	}
	if full {
		vars["BodyHtml"] = BodyHTML(record.Body)
	}
	if record.Cta != nil {
		if href := webLink(record.Cta.Href, true); href != "" {
			vars["CtaHref"] = href
			vars["CtaLabel"] = record.Cta.Label
			if record.Cta.Label == "" {
				vars["CtaLabel"] = "Bağlantıyı aç"
			}
		}
	}
	return vars
}

// AudienceKey is a send's audience as mail_tasks.source_audience keeps it:
// members, or teams: and the named teams sorted, each once, so the same
// audience asked in another order is the same send.
func AudienceKey(kind string, teams []string) string {
	if kind == AudienceTypeMembers {
		return AudienceTypeMembers
	}
	sorted := append([]string(nil), teams...)
	for i := range sorted {
		sorted[i] = strings.TrimSpace(sorted[i])
	}
	slices.Sort(sorted)
	return AudienceTypeTeams + ":" + strings.Join(slices.Compact(sorted), ",")
}
