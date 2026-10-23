package mailer

import (
	"context"
	"html"
	"strings"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The visible unsubscribe line (ADR 0072, industry comparison 10; hub ticket
// abonelik-cikisi 03): Google and Yahoo want a bulk mail's body to carry a
// visible unsubscribe link besides List-Unsubscribe, and Postmark adds one to
// every broadcast mail itself. While SKYMAIL_MAIL_PURPOSE is on, a mail of the
// duyuru stream whose row has an unsubscribe link (list_unsubscribe_url) gets
// a short line at its end with that link, in the HTML and in the plain text,
// when that part does not carry the link already: a template that writes
// {{.UnsubscribeUrl}} itself (its footer, ticket 05) gets nothing added. The
// line is added as the mail is sent, after it was rendered and queued: the
// row and the send's content page keep the mail as it was rendered, and the
// link never gets a click address (only the links registered when the send
// was queued do).
//
// The link is the row's, as ValidUnsubscribeURL holds it: an absolute https
// URL of visible ASCII without <, >, quote or comma, so it is written into
// the HTML attribute escaped and into the plain text between < and > as it
// is.

// Footer texts: short and functional (the plain text names the link in
// angle brackets, RFC 3986 appendix C).
const (
	footerText      = "SKY LAB duyurularını almak istemiyorsan:"
	footerLinkLabel = "Abonelikten çık"
)

// withUnsubscribeFooter is job as it is sent: with the visible unsubscribe
// line when the switch is on, the row has a link and the mail is in the
// duyuru stream (lane, or read here when the sender lanes are all off and
// laneOf did not). Any other mail is job unchanged.
func (s *emailSender) withUnsubscribeFooter(ctx context.Context, lane Lane, job database.MailQueue) (database.MailQueue, error) {
	if !s.lanes.Purpose || job.ListUnsubscribeUrl == nil || !ValidUnsubscribeURL(*job.ListUnsubscribeUrl) {
		return job, nil
	}
	if lane == "" {
		var err error
		if lane, err = s.streamOf(ctx, job); err != nil {
			return job, err
		}
	}
	if lane != LaneDuyuru {
		return job, nil
	}
	link := *job.ListUnsubscribeUrl
	job.Body = PlainWithUnsubscribeFooter(job.Body, link)
	if job.BodyHtml != nil {
		withLine := HTMLWithUnsubscribeFooter(*job.BodyHtml, link)
		job.BodyHtml = &withLine
	}
	return job, nil
}

// PlainWithUnsubscribeFooter is a mail's plain text with the unsubscribe
// line at its end, or as it is when it carries link already.
func PlainWithUnsubscribeFooter(body, link string) string {
	if strings.Contains(body, link) {
		return body
	}
	line := footerText + " " + footerLinkLabel + " <" + link + ">"
	body = strings.TrimRight(body, "\r\n")
	if body == "" {
		return line + "\n"
	}
	return body + "\n\n" + line + "\n"
}

// HTMLWithUnsubscribeFooter is a mail's HTML with the unsubscribe line
// before its last </body> (lastBodyClose; at its end when it has none), or as it is when it
// carries link already: as written, or with & escaped, as html/template
// writes a URL into an attribute.
func HTMLWithUnsubscribeFooter(body, link string) string {
	escaped := html.EscapeString(link)
	if strings.Contains(body, link) || strings.Contains(body, escaped) {
		return body
	}
	line := `<p style="margin:24px 0 0;font-size:12px;line-height:18px;color:#6b7280">` +
		html.EscapeString(footerText) + ` <a href="` + escaped + `" style="color:#6b7280">` +
		html.EscapeString(footerLinkLabel) + `</a></p>`
	if at := lastBodyClose(body); at >= 0 {
		return body[:at] + line + body[at:]
	}
	return body + line
}

// lastBodyClose is the byte offset in body of its last </body> end tag (any
// ASCII case, white space allowed before the >), or -1. It compares bytes
// and folds ASCII letters only: lower-casing the whole body would move every
// offset after a letter whose lower case has another length in UTF-8 (a
// Turkish İ is two bytes, its i one).
func lastBodyClose(body string) int {
	const tag = "</body"
	for at := len(body) - len(tag); at >= 0; at-- {
		if !asciiFoldEqual(body[at:at+len(tag)], tag) {
			continue
		}
		end := at + len(tag)
		for end < len(body) && (body[end] == ' ' || body[end] == '\t' || body[end] == '\n' || body[end] == '\r' || body[end] == '\f') {
			end++
		}
		if end < len(body) && body[end] == '>' {
			return at
		}
	}
	return -1
}

// asciiFoldEqual says s equals lower, a lower-case ASCII string, with ASCII
// letters compared in either case; any other byte must be the same.
func asciiFoldEqual(s, lower string) bool {
	for i := 0; i < len(lower); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}
