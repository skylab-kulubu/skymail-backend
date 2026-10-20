package mailer

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
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

// permanent says the refusal is about this recipient's address, so the same
// mail to them is refused again: a 5xx whose enhanced code is a status of the
// recipient's address (recipientAddressStatus), to RCPT TO or at the end of
// DATA; or, from a relay that gives no enhanced code, a 550, 551 or 553 to
// RCPT TO (mailbox unavailable, not local, name not allowed: RFC 5321
// §4.2.2). Every other refusal is tried again later, a 5xx included, so a
// refusal of the sender, the account, the tenant or the relay's policy cannot
// fail every recipient of a list at once:
//
//   - a 4xx (RFC 5321 §4.2.1: transient);
//   - a 5xx to MAIL FROM (Exchange Online's 554 5.2.0
//     SubmissionQuotaExceeded, a 5.7.x sender refusal);
//   - a 5xx to RCPT TO or at the end of DATA that is 5.2.x (mailbox or
//     quota), 5.3.x (mail system), 5.7.x (policy, relay access denied), a
//     5.1.x about the sender or the account (5.1.7, 5.1.8, Exchange Online's
//     5.1.90 daily recipient limit), of any other class, or has no enhanced
//     code and is not one of the three above;
//   - a 5xx to anything else.
func (r smtpReply) permanent() bool {
	if r.code < 500 || r.code >= 600 {
		return false
	}
	if r.stage != stageRcptTo && r.stage != stageData {
		return false
	}
	if r.enhanced != "" {
		return recipientAddressStatus(r.enhanced)
	}
	return r.stage == stageRcptTo && (r.code == 550 || r.code == 551 || r.code == 553)
}

// recipientAddressStatus says an enhanced code is a permanent status of the
// recipient's address: 5.1.0 to 5.1.6 (RFC 3463: other, bad mailbox, bad
// system, bad syntax, ambiguous, valid, moved) or 5.1.10 (null MX, RFC 7505;
// Exchange Online's RecipientNotFound). The rest of 5.1.x is not: 5.1.7 and
// 5.1.8 are the sender's mailbox and system (RFC 3463), which Postfix
// answers at RCPT TO by default (smtpd_delay_reject), and Exchange Online
// gives 5.1.8 to a blocked sending account and 5.1.90 when it has reached its
// daily recipient limit; 5.1.9 and any code not listed are unknown here.
// Each would fail every recipient of a list at once.
func recipientAddressStatus(enhanced string) bool {
	detail, ok := strings.CutPrefix(enhanced, "5.1.")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(detail)
	if err != nil || n < 0 {
		return false
	}
	return n <= 6 || n == 10
}

// enhancedCode finds an RFC 3463 code right after the reply code, for a relay
// that does not announce ENHANCEDSTATUSCODES (go-mail reads it only then).
// Newer Go releases quote the reply's text in net/textproto's error
// (`550 "5.1.1 …"`), older ones do not.
var enhancedCode = regexp.MustCompile(`\b[45]\d\d[ -]"?([245]\.\d{1,3}\.\d{1,3})\b`)

// smtpFailure is how a send the relay did not take fails. A refusal of the
// recipient (smtpReply.permanent) fails the row for good at once
// (permanentError): the address is refused again in four minutes. Any other
// reply, or a failure with no reply (the connection dropped, the send budget
// ran out), is tried again later, up to maxSendAttempts.
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
		return stageMailFrom
	case mail.ErrSMTPRcptTo:
		return stageRcptTo
	case mail.ErrSMTPData, mail.ErrSMTPDataClose, mail.ErrWriteContent:
		return stageData
	default:
		return "SMTP"
	}
}

// The stages a refusal is classified by.
const (
	stageMailFrom = "MAIL FROM"
	stageRcptTo   = "RCPT TO"
	stageData     = "DATA"
)

// reservedDomainError is the error a recipient in a reserved domain is
// written failed with: it was never sent. mail_task_status() counts such a
// row skipped, not failed (20261020150000), by this prefix.
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
