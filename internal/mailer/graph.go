package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	netmail "net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/dsn"
)

// The Microsoft Graph sendMail path (a lane with TransportGraph): POST
// /users/{mailbox}/sendMail with the mail as JSON and saveToSentItems false,
// so no copy of a login code or of any mail is kept in the mailbox (Yusuf,
// 2026-10-20). Graph answers 202 with no body: it gives back neither the
// message nor its Message-ID, and a mail's internetMessageId can be set on a
// draft only, which Sent Items would keep. So the mail carries its queue row
// in X-SkyMail-Queue-Id, <id@SMTP_FQDN> like SkyMail's Message-ID, and a
// bounce's returned headers name the row by it (dsn.QueueIDHeader). The
// response's request-id is logged with the row, for Microsoft's support.
//
// JSON has one body: the HTML when the mail has one (Exchange writes the
// plain-text alternative of an HTML mail itself), the plain text otherwise.

// DefaultGraphURL is Microsoft Graph's v1.0 endpoint.
const DefaultGraphURL = "https://graph.microsoft.com/v1.0"

// graphRecipient, graphMessage and graphSendMail are sendMail's request body.
type graphRecipient struct {
	EmailAddress graphAddress `json:"emailAddress"`
}

type graphAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type graphHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphMessage struct {
	Subject                string           `json:"subject"`
	Body                   graphBody        `json:"body"`
	From                   graphRecipient   `json:"from"`
	ToRecipients           []graphRecipient `json:"toRecipients"`
	ReplyTo                []graphRecipient `json:"replyTo,omitempty"`
	InternetMessageHeaders []graphHeader    `json:"internetMessageHeaders"`
}

type graphSendMail struct {
	Message graphMessage `json:"message"`
	// SaveToSentItems is always false and always written.
	SaveToSentItems bool `json:"saveToSentItems"`
}

func recipientOf(address *netmail.Address) graphRecipient {
	return graphRecipient{EmailAddress: graphAddress{Name: address.Name, Address: address.Address}}
}

// queueHeader is the value of X-SkyMail-Queue-Id: the row as its Message-ID
// names it.
func queueHeader(job database.MailQueue, fqdn string) string {
	return fmt.Sprintf("<%s@%s>", job.ID.String(), fqdn)
}

// recipientAddress is the row's recipient as Graph takes it, or the
// permanent invalid_recipient failure the SMTP path gives an address go-mail
// cannot parse.
func recipientAddress(job database.MailQueue) (*netmail.Address, error) {
	address, err := netmail.ParseAddress(strings.TrimSpace(job.RecipientEmail))
	if err != nil || address.Name != "" {
		return nil, permanentError{errors.New(invalidRecipientError)}
	}
	address.Name = job.RecipientFullName
	return address, nil
}

// graphRequest is the sendMail body of job from lane's mailbox.
func (s *emailSender) graphRequest(ctx context.Context, job database.MailQueue, lane LaneConfig) ([]byte, error) {
	to, err := recipientAddress(job)
	if err != nil {
		return nil, err
	}
	body := graphBody{ContentType: "Text", Content: job.Body}
	if job.BodyHtml != nil {
		body = graphBody{ContentType: "HTML", Content: s.outgoingHTML(ctx, job)}
	}
	message := graphMessage{
		Subject:                job.Subject,
		Body:                   body,
		From:                   recipientOf(lane.From),
		ToRecipients:           []graphRecipient{recipientOf(to)},
		InternetMessageHeaders: []graphHeader{{Name: dsn.QueueIDHeader, Value: queueHeader(job, s.config.FQDN)}},
	}
	if s.lanes.ReplyTo != nil {
		message.ReplyTo = []graphRecipient{recipientOf(s.lanes.ReplyTo)}
	}
	return json.Marshal(graphSendMail{Message: message, SaveToSentItems: false})
}

// sendGraph sends job from lane's mailbox with Graph sendMail.
func (s *emailSender) sendGraph(ctx context.Context, job database.MailQueue, lane Lane, config LaneConfig) error {
	payload, err := s.graphRequest(ctx, job, config)
	if err != nil {
		return err
	}
	send, cancel := context.WithTimeout(ctx, s.sendBudget)
	defer cancel()
	token, err := s.graphTokens.Token(send)
	if err != nil {
		return fmt.Errorf("send failed: graph token: %w", err)
	}
	endpoint := s.graphURL + "/users/" + url.PathEscape(config.From.Address) + "/sendMail"
	req, err := http.NewRequestWithContext(send, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Graph echoes it and Microsoft's support finds the request by it.
	req.Header.Set("client-request-id", job.ID.String())
	resp, err := s.graphClient.Do(req)
	if err != nil {
		// Without the URL's error text: only that it did not answer.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("send failed: graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		s.logger.Debug().Str("job_id", job.ID.String()).Str("lane", string(lane)).
			Str("graph_request_id", resp.Header.Get("request-id")).
			Msg("Graph took the mail (202)")
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	failure := graphFailure(resp.StatusCode, body, resp.Header, time.Now())
	if resp.StatusCode == http.StatusUnauthorized {
		s.graphTokens.Invalidate(token)
	}
	s.logger.Debug().Str("job_id", job.ID.String()).Str("lane", string(lane)).
		Str("graph_request_id", resp.Header.Get("request-id")).Str("graph_error", failure.Error()).
		Msg("Graph refused the mail")
	return failure
}

// graphReply is Graph's refusal of a send: its HTTP status and error code,
// never its message, which can quote the address. RetryAfter is what its
// Retry-After asked for.
type graphReply struct {
	status     int
	code       string
	retryAfter time.Duration
	permanent  bool
}

func (r graphReply) Error() string {
	kind := "graph_deferred"
	if r.permanent {
		kind = "graph_rejected"
	}
	reply := fmt.Sprintf("%s: %d", kind, r.status)
	if r.code != "" {
		reply += " " + r.code
	}
	if r.retryAfter > 0 {
		reply += " retry-after " + r.retryAfter.String()
	}
	return reply
}

// RetryAfter is how long Graph asked the send to wait (0: it did not say).
func (r graphReply) RetryAfter() time.Duration { return r.retryAfter }

// graphErrorCode keeps a Graph error code only if it looks like one
// (ErrorInvalidRecipients, ApplicationThrottled).
var graphErrorCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)

// graphFailure classifies Graph's answer to a send, as smtpFailure classifies
// a relay's: permanent only when the same mail would be refused again,
//
//   - 400 ErrorInvalidRecipients: the recipient's address is not one
//     Exchange can send to (permanent; nothing was sent, so nothing is
//     suppressed, as with an address go-mail cannot parse);
//   - 413: the mail is too large for the request (permanent);
//
// and transient otherwise, a 4xx included, so a refusal of the application,
// the mailbox or the tenant cannot fail every recipient of a list at once:
// 401 (the token; it is dropped), 403 (Mail.Send not granted yet, the RBAC
// scope still propagating), 404 (the mailbox), another 400, 429 and 503
// (throttled: their Retry-After is kept and the retry waits at least that
// long), any other 5xx.
func graphFailure(status int, body []byte, header http.Header, now time.Time) error {
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	reply := graphReply{status: status, retryAfter: retryAfter(header.Get("Retry-After"), now)}
	if graphErrorCode.MatchString(parsed.Error.Code) {
		reply.code = parsed.Error.Code
	}
	switch {
	case status == http.StatusBadRequest && reply.code == "ErrorInvalidRecipients",
		status == http.StatusRequestEntityTooLarge:
		reply.permanent = true
		return permanentError{reply}
	}
	return reply
}

// maxRetryAfter bounds how long a Retry-After may hold a row back.
const maxRetryAfter = time.Hour

// retryAfter reads a Retry-After header: seconds or an HTTP date (RFC 9110
// §10.2.3); 0 when absent or unreadable.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var wait time.Duration
	if seconds, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now).Round(time.Second)
	}
	if wait <= 0 {
		return 0
	}
	return min(wait, maxRetryAfter)
}
