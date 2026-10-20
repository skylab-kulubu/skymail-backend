// Package dsn reads a delivery status notification (RFC 3464, carried in an
// RFC 6522 multipart/report): the report a mail system sends back when a mail
// could not be delivered (an NDR) or is delayed. It reads only the machine
// part, message/delivery-status, and the headers of the returned message;
// the human-readable part, the subject and any HTML are never read, since
// Exchange Online words and localises them as it likes (hub spec
// skymail-bounce-isleme §3).
//
// Nothing here keeps the report's text: a recipient's Diagnostic-Code is
// reduced to its reply code, its enhanced code and Exchange Online's AS(…)
// token, as the SMTP path keeps a relay's refusal (mailer.smtpReply). The
// addresses are returned for matching against the queue row and are not to
// be logged.
package dsn

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ErrNotDSN says the message is not a delivery status notification: its
// top-level type is not multipart/report with report-type=delivery-status,
// or it has no message/delivery-status part. An auto-reply, an out-of-office
// or a person's mail lands here, and so does a remote system's plain-text
// bounce, which is not read (the hub spec names the deviation: no heuristic
// parsing).
var ErrNotDSN = errors.New("not a delivery status notification")

// Limits on what is read. A report's machine part is a few hundred bytes per
// recipient; anything larger is not a report this package reads.
const (
	maxStatusPart  = 256 << 10
	maxHeaderPart  = 256 << 10
	maxRecipients  = 100
	maxReportParts = 20
)

// Report is what a delivery status notification says.
type Report struct {
	// MessageID is the report's own Message-ID, as written (with its angle
	// brackets), or empty.
	MessageID string
	// OriginalMessageID is the Message-ID of the returned message (its
	// message/rfc822 or text/rfc822-headers part), as written, or empty when
	// the report returned no headers or the headers carry none.
	OriginalMessageID string
	// InReplyTo and References are the report's own headers, as written.
	// Exchange Online sets them to the returned message's Message-ID.
	InReplyTo  string
	References string
	// Recipients are the per-recipient blocks of message/delivery-status,
	// in order.
	Recipients []Recipient
}

// Recipient is one per-recipient block.
type Recipient struct {
	// FinalRecipient and OriginalRecipient are the addresses of an rfc822
	// (or utf-8) address type, lower-cased, without angle brackets; empty
	// for another type or a missing field.
	FinalRecipient    string
	OriginalRecipient string
	// Action is failed, delayed, delivered, relayed or expanded, lower-cased.
	Action string
	// Status is the block's RFC 3463 code (Status, or the Diagnostic-Code's
	// when Status is missing or only says the class): 5.1.10.
	Status string
	// SMTPCode is the Diagnostic-Code's reply code (550), 0 if none.
	SMTPCode int
	// ASCode is the digits of Exchange Online's AS(…) token in the
	// Diagnostic-Code (201806281: Directory-Based Edge Blocking found no such
	// recipient), empty if none.
	ASCode string
}

// Parse reads a message and returns its report. A message that is not a
// delivery status notification is ErrNotDSN; a report that cannot be read
// is another error.
func Parse(r io.Reader) (*Report, error) {
	msg, err := mail.ReadMessage(bufio.NewReader(r))
	if err != nil {
		return nil, fmt.Errorf("read the message: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/report" || !strings.EqualFold(params["report-type"], "delivery-status") {
		return nil, ErrNotDSN
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, ErrNotDSN
	}
	report := &Report{
		MessageID:  strings.TrimSpace(msg.Header.Get("Message-Id")),
		InReplyTo:  strings.TrimSpace(msg.Header.Get("In-Reply-To")),
		References: strings.TrimSpace(msg.Header.Get("References")),
	}

	parts := multipart.NewReader(msg.Body, boundary)
	sawStatus := false
	for n := 0; ; n++ {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the report's parts: %w", err)
		}
		if n >= maxReportParts {
			return nil, errors.New("the report has too many parts")
		}
		partType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			partType = "text/plain"
		}
		body := decoded(part)
		switch partType {
		case "message/delivery-status", "message/global-delivery-status":
			if sawStatus {
				continue
			}
			sawStatus = true
			recipients, err := readStatus(body)
			if err != nil {
				return nil, err
			}
			report.Recipients = recipients
		case "message/rfc822", "text/rfc822-headers", "message/global", "message/global-headers":
			if report.OriginalMessageID == "" {
				report.OriginalMessageID = originalMessageID(body)
			}
		}
	}
	if !sawStatus {
		return nil, ErrNotDSN
	}
	return report, nil
}

// decoded is a part's body with its Content-Transfer-Encoding undone:
// mime/multipart undoes quoted-printable itself, base64 is undone here.
func decoded(part *multipart.Part) io.Reader {
	if strings.EqualFold(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding")), "base64") {
		return base64.NewDecoder(base64.StdEncoding, &skipSpace{r: part})
	}
	return part
}

// skipSpace drops the line breaks and spaces base64 is wrapped with.
type skipSpace struct{ r io.Reader }

func (s *skipSpace) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		kept := 0
		for _, b := range p[:n] {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				p[kept] = b
				kept++
			}
		}
		if kept > 0 || err != nil {
			return kept, err
		}
	}
}

// readStatus reads message/delivery-status: the per-message fields, then a
// block per recipient, each a run of header fields ended by a blank line.
func readStatus(body io.Reader) ([]Recipient, error) {
	limited := &io.LimitedReader{R: body, N: maxStatusPart + 1}
	fields := textproto.NewReader(bufio.NewReader(limited))
	var blocks []textproto.MIMEHeader
	for {
		block, err := readBlock(fields)
		if len(block) > 0 {
			blocks = append(blocks, block)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the delivery status: %w", err)
		}
		if len(blocks) > maxRecipients+1 {
			return nil, errors.New("the delivery status has too many recipients")
		}
	}
	if limited.N <= 0 {
		return nil, errors.New("the delivery status is too long")
	}
	var recipients []Recipient
	for i, block := range blocks {
		// The first block is the per-message fields (Reporting-MTA,
		// Arrival-Date); a block is a recipient's when it names one.
		if i == 0 && block.Get("Final-Recipient") == "" && block.Get("Action") == "" {
			continue
		}
		recipients = append(recipients, recipient(block))
	}
	return recipients, nil
}

// readBlock reads one block of fields, skipping the blank lines before it.
// It returns io.EOF with the last block when the part ends.
func readBlock(r *textproto.Reader) (textproto.MIMEHeader, error) {
	for {
		peek, err := r.R.Peek(1)
		if err != nil {
			return nil, io.EOF
		}
		if peek[0] != '\r' && peek[0] != '\n' {
			break
		}
		if _, err := r.R.ReadByte(); err != nil {
			return nil, io.EOF
		}
	}
	block, err := r.ReadMIMEHeader()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return block, io.EOF
	}
	return block, err
}

// The codes a Diagnostic-Code of type smtp carries: the reply code at its
// start, an RFC 3463 code after it, and Exchange Online's AS(…) token.
var (
	diagnosticReply    = regexp.MustCompile(`^\s*([245]\d\d)(?:[ -]|$)`)
	diagnosticEnhanced = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)
	asToken            = regexp.MustCompile(`\bAS\((\d{1,12})\)`)
	statusCode         = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}$`)
)

func recipient(block textproto.MIMEHeader) Recipient {
	rcpt := Recipient{
		FinalRecipient:    address(block.Get("Final-Recipient")),
		OriginalRecipient: address(block.Get("Original-Recipient")),
		Action:            strings.ToLower(firstWord(block.Get("Action"))),
	}
	status := firstWord(block.Get("Status"))
	if !statusCode.MatchString(status) {
		status = ""
	}
	diagType, diag, _ := strings.Cut(block.Get("Diagnostic-Code"), ";")
	var diagStatus string
	if strings.EqualFold(strings.TrimSpace(diagType), "smtp") {
		if m := diagnosticReply.FindStringSubmatch(diag); m != nil {
			rcpt.SMTPCode, _ = strconv.Atoi(m[1])
		}
		if m := diagnosticEnhanced.FindStringSubmatch(diag); m != nil {
			diagStatus = m[1]
		}
		if m := asToken.FindStringSubmatch(diag); m != nil {
			rcpt.ASCode = m[1]
		}
	}
	// Status is required (RFC 3464 §2.3.4) and is what decides. Some MTAs
	// write only the class there (5.0.0) and the code in the
	// Diagnostic-Code: the latter is taken when it is of the same class.
	switch {
	case status == "":
		rcpt.Status = diagStatus
	case strings.HasSuffix(status, ".0.0") && diagStatus != "" && diagStatus[0] == status[0]:
		rcpt.Status = diagStatus
	default:
		rcpt.Status = status
	}
	return rcpt
}

// address reads an address field, "rfc822; user@example.com": the address of
// an rfc822 or utf-8 type, lower-cased, or empty.
func address(field string) string {
	kind, addr, ok := strings.Cut(field, ";")
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "rfc822", "utf-8":
	default:
		return ""
	}
	addr = strings.TrimSpace(addr)
	addr = strings.TrimSuffix(strings.TrimPrefix(addr, "<"), ">")
	if !strings.Contains(addr, "@") || strings.ContainsAny(addr, " \t<>") {
		return ""
	}
	return strings.ToLower(addr)
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// originalMessageID reads the Message-ID of a returned message's headers.
func originalMessageID(body io.Reader) string {
	header, err := textproto.NewReader(bufio.NewReader(io.LimitReader(body, maxHeaderPart))).ReadMIMEHeader()
	if err != nil && len(header) == 0 {
		return ""
	}
	return strings.TrimSpace(header.Get("Message-Id"))
}

// QueueID is the queue row the report is about: the id in the returned
// message's Message-ID, <id@fqdn>, as SkyMail writes it (mailer emailSender).
// The returned headers decide when they carry a Message-ID; without one, the
// report's own In-Reply-To, then References. The domain must be fqdn
// (case-insensitively) and the id a UUID in its canonical form. ok is false
// when the deciding header names another message: a mail SkyMail did not
// send (the rotator's alarm mail, Keycloak's own SMTP), or none at all.
func (r *Report) QueueID(fqdn string) (id uuid.UUID, ok bool) {
	var source string
	switch {
	case r.OriginalMessageID != "":
		source = r.OriginalMessageID
	case r.InReplyTo != "":
		source = r.InReplyTo
	default:
		source = r.References
	}
	for _, msgID := range messageIDs(source) {
		if id, ok := queueID(msgID, fqdn); ok {
			return id, true
		}
	}
	return uuid.Nil, false
}

// messageIDs splits a header into its <…> message ids.
func messageIDs(header string) []string {
	var ids []string
	for {
		start := strings.IndexByte(header, '<')
		if start < 0 {
			return ids
		}
		end := strings.IndexByte(header[start:], '>')
		if end < 0 {
			return ids
		}
		ids = append(ids, header[start+1:start+end])
		header = header[start+end+1:]
	}
}

func queueID(msgID, fqdn string) (uuid.UUID, bool) {
	at := strings.LastIndexByte(msgID, '@')
	if at < 0 || fqdn == "" || !strings.EqualFold(msgID[at+1:], fqdn) {
		return uuid.Nil, false
	}
	local := msgID[:at]
	if len(local) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(local)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// Kind is what a recipient's block means for the address.
type Kind string

const (
	// KindHard is a permanent failure of the address itself: the address is
	// suppressed (hub spec §4).
	KindHard Kind = "hard"
	// KindSoft is any other failure: recorded, never suppressed.
	KindSoft Kind = "soft"
	// KindDelay is a delay report: recorded, never suppressed.
	KindDelay Kind = "delay"
	// KindNone is a block that reports no failure (delivered, relayed,
	// expanded) or no action: nothing is recorded.
	KindNone Kind = ""
)

// ExchangeRecipientUnknownAS is the AS(…) token Exchange Online's
// Directory-Based Edge Blocking gives with 550 5.4.1 when the receiving
// tenant has no such recipient: in practice the address does not exist.
const ExchangeRecipientUnknownAS = "201806281"

// Classify says what a recipient's block means. A failure is hard when its
// code is a permanent status of the recipient's address
// (RecipientAddressStatus), or 5.4.1 with AS(201806281); any other failure is
// soft, a 4.x.x one included (a delivery given up after retrying, 4.4.7). A
// delay is a delay whatever its code.
func Classify(r Recipient) Kind {
	switch r.Action {
	case "failed":
		if RecipientAddressStatus(r.Status) || (r.Status == "5.4.1" && r.ASCode == ExchangeRecipientUnknownAS) {
			return KindHard
		}
		return KindSoft
	case "delayed":
		return KindDelay
	default:
		return KindNone
	}
}

// RecipientAddressStatus says an enhanced code is a permanent status of the
// recipient's address: 5.1.0 to 5.1.6 (RFC 3463: other, bad mailbox, bad
// system, bad syntax, ambiguous, valid, moved) or 5.1.10 (null MX, RFC 7505;
// Exchange Online's RecipientNotFound). The rest of 5.1.x is not: 5.1.7 and
// 5.1.8 are the sender's mailbox and system (RFC 3463), which Postfix
// answers at RCPT TO by default (smtpd_delay_reject), and Exchange Online
// gives 5.1.8 to a blocked sending account and 5.1.90 when it has reached its
// daily recipient limit; 5.1.9 and any code not listed are unknown here.
// Each would fail every recipient of a list at once.
//
// The SMTP path (mailer.smtpReply) and the report path share it, so a relay's
// refusal and a report suppress the same codes.
func RecipientAddressStatus(enhanced string) bool {
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
