package mailer

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/wneessen/go-mail"
)

// smtpReply is a relay's refusal of a send: the stage it came at and the
// reply's codes. It never holds the relay's text, which may echo the address
// (go-mail's own error names the recipient and the Message-ID as well): what
// a row records is logged and shown on the send's page.
type smtpReply struct {
	stage    string
	code     int
	enhanced string
}

func (r smtpReply) Error() string {
	kind := "smtp_deferred"
	if r.permanent() {
		kind = "smtp_rejected"
	}
	reply := fmt.Sprintf("%s: %s %d", kind, r.stage, r.code)
	if r.enhanced != "" {
		reply += " " + r.enhanced
	}
	return reply
}

// permanent says the reply is a 5xx: the relay's final answer (RFC 5321
// §4.2.1), which the same mail will get again. A 4xx is transient.
func (r smtpReply) permanent() bool { return r.code >= 500 && r.code < 600 }

// enhancedCode finds an RFC 3463 code right after the reply code, for a relay
// that does not announce ENHANCEDSTATUSCODES (go-mail reads it only then).
// Newer Go releases quote the reply's text in net/textproto's error
// (`550 "5.1.1 …"`), older ones do not.
var enhancedCode = regexp.MustCompile(`\b[45]\d\d[ -]"?([245]\.\d{1,3}\.\d{1,3})\b`)

// smtpFailure is how a send the relay did not take fails. A 5xx reply fails
// the row for good at once (permanentError): a refused address or a refused
// mail is refused again in four minutes. A 4xx reply, or a failure with no
// reply (the connection dropped, the send budget ran out), is tried again
// later, up to maxSendAttempts.
func smtpFailure(err error) error {
	var sendErr *mail.SendError
	if !errors.As(err, &sendErr) {
		return fmt.Errorf("send failed: %w", err)
	}
	code := sendErr.ErrorCode()
	if code < 400 || code >= 600 {
		// No reply code: go-mail's text, without the recipient and the
		// Message-ID it appends.
		text, _, _ := strings.Cut(sendErr.Error(), ", affected recipient(s): ")
		text, _, _ = strings.Cut(text, ", affected message ID: ")
		return errors.New("send failed: " + text)
	}
	reply := smtpReply{stage: smtpStage(sendErr.Reason), code: code, enhanced: sendErr.EnhancedStatusCode()}
	if reply.enhanced == "" {
		if m := enhancedCode.FindStringSubmatch(sendErr.Error()); m != nil {
			reply.enhanced = m[1]
		}
	}
	if reply.permanent() {
		return permanentError{reply}
	}
	return reply
}

// smtpStage names the command a refusal answered.
func smtpStage(reason mail.SendErrReason) string {
	switch reason {
	case mail.ErrSMTPMailFrom:
		return "MAIL FROM"
	case mail.ErrSMTPRcptTo:
		return "RCPT TO"
	case mail.ErrSMTPData, mail.ErrSMTPDataClose, mail.ErrWriteContent:
		return "DATA"
	default:
		return "SMTP"
	}
}

// reservedDomainError is the error a recipient in a reserved domain is
// written failed with: it was never sent.
const reservedDomainError = "reserved_domain: not sent, the address is in a domain reserved by RFC 2606/6761"

// reservedDomains are the names RFC 2606 and RFC 6761 reserve for examples
// and tests: no mail is ever delivered there, and a relay refuses it (or
// worse, tries). A name or any name under it.
var reservedDomains = []string{"example.com", "example.net", "example.org", "example", "test", "invalid", "localhost"}

// WithReservedDomainsRefused writes a recipient in a reserved domain
// (reservedDomain) failed with reservedDomainError when a send is queued,
// instead of queueing a mail the relay can only refuse. Without it such a
// recipient is queued like any other: the tests send to example.com through a
// fake relay.
func WithReservedDomainsRefused() Option {
	return func(m *mailerImpl) { m.refuseReserved = true }
}

// reservedDomain says address's domain is one of reservedDomains or under
// one. The domain is what follows the last @.
func reservedDomain(address string) bool {
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return false
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(address[at+1:])), ".")
	for _, reserved := range reservedDomains {
		if domain == reserved || strings.HasSuffix(domain, "."+reserved) {
			return true
		}
	}
	return false
}
