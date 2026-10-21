package mailer

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// One-click unsubscribe (RFC 8058; hub ticket skymail-bounce-isleme 11). A
// bulk mail whose recipient core gave an unsubscribe link for (the row's
// list_unsubscribe_url) carries
//
//	List-Unsubscribe: <https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=…>
//	List-Unsubscribe-Post: List-Unsubscribe=One-Click
//
// only while SKYMAIL_LIST_UNSUBSCRIBE is on, and only from the duyuru@
// mailbox: the duyuru lane sent with graph or xoauth2. Account mail (hesap@)
// and system mail (bilgi@) never carry it, nor does the off path. The link is
// core's (its communication preferences, which the Account center shows):
// core signs it and turns the categories off on the mail client's POST;
// SkyMail never holds the key and keeps no preference of its own. Graph's JSON
// sendMail carries only X- headers, so on the graph transport such a mail is
// sent as MIME (graph.go), the others as before.

// ListUnsubscribeEnv switches the headers on: off (the default, also when
// unset) or on.
const ListUnsubscribeEnv = "SKYMAIL_LIST_UNSUBSCRIBE"

// MaxUnsubscribeURLLength bounds a link, as mail_queue's check does: the
// header is written on one line, which RFC 5322 caps at 998 characters. Core's
// link for a few categories is about 150.
const MaxUnsubscribeURLLength = 960

// listUnsubscribeFromEnv reads SKYMAIL_LIST_UNSUBSCRIBE; anything but off, on
// or nothing stops startup.
func listUnsubscribeFromEnv(getenv func(string) string) (bool, error) {
	switch raw := strings.ToLower(strings.TrimSpace(getenv(ListUnsubscribeEnv))); raw {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be off or on, not %q", ListUnsubscribeEnv, raw)
	}
}

// ValidUnsubscribeURL says link may go into List-Unsubscribe: an absolute
// https URL with a host and no user, at most MaxUnsubscribeURLLength bytes of
// visible ASCII without the characters that end or split the header's
// <…> list (<, >, comma, quote). Nothing a caller sends can then break the
// header or add another.
func ValidUnsubscribeURL(link string) bool {
	if len(link) == 0 || len(link) > MaxUnsubscribeURLLength || !strings.HasPrefix(link, "https://") {
		return false
	}
	for i := 0; i < len(link); i++ {
		c := link[i]
		if c < 0x21 || c > 0x7e || c == '<' || c == '>' || c == ',' || c == '"' {
			return false
		}
	}
	parsed, err := url.Parse(link)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

// listUnsubscribe is the link job's mail carries in List-Unsubscribe, or ""
// for none: the switch on, the duyuru lane sent from its own mailbox, and a
// link on the row that is still one (the check on the column holds it, this
// holds an older row too).
func (s *emailSender) listUnsubscribe(lane Lane, config LaneConfig, job database.MailQueue) string {
	if !s.lanes.ListUnsubscribe || lane != LaneDuyuru || config.Transport == TransportOff || job.ListUnsubscribeUrl == nil {
		return ""
	}
	if !ValidUnsubscribeURL(*job.ListUnsubscribeUrl) {
		return ""
	}
	return *job.ListUnsubscribeUrl
}

// setListUnsubscribe writes the two headers of RFC 8058 on msg. The link is
// written as it is, on one line (ValidUnsubscribeURL holds it to visible
// ASCII within the line's length): go-mail would fold it after the colon.
func setListUnsubscribe(msg *mail.Msg, link string) {
	msg.SetGenHeaderPreformatted(mail.HeaderListUnsubscribe, "<"+link+">")
	msg.SetGenHeader(mail.HeaderListUnsubscribePost, "List-Unsubscribe=One-Click")
}
