package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/dsn"
	"github.com/wneessen/go-mail"
)

// A queue row is one delivery to one person over one channel
// (mail_queue.channel). The engine (the lanes, the lease and its fencing, the
// retries, the send budget) does not care how a row goes out: a worker hands
// each row it takes to the sender of the row's channel. E-mail is the only
// channel yet; recipient_email is its address.
const ChannelEmail = "email"

// invalidRecipientError is the error a row whose name and address go-mail
// cannot parse into a To header fails with: the class alone, as with
// reservedDomainError; the row holds the name and the address.
const invalidRecipientError = "invalid_recipient: not sent, the recipient's name and address could not be parsed"

// ChannelSender sends the rows of one channel. Each worker makes its own of
// every channel at startup and sends one row at a time with it, so what a
// channel keeps between sends (the SMTP client) is the worker's alone.
//
// Send's error is how the send went: nil, sent; a permanentError, failed for
// good at once (retrying cannot fix it: an address go-mail cannot parse, a
// relay's 5xx); any other, tried again later, up to maxSendAttempts. An error while ctx has ended is the process going away,
// not a failed attempt: the row is sent again after its lease runs out.
type ChannelSender interface {
	Send(ctx context.Context, job database.MailQueue) error
}

// newChannelSender makes one worker's sender of a channel.
type newChannelSender func() (ChannelSender, error)

// channelSenderFunc is a ChannelSender that is only a function.
type channelSenderFunc func(ctx context.Context, job database.MailQueue) error

func (f channelSenderFunc) Send(ctx context.Context, job database.MailQueue) error {
	return f(ctx, job)
}

// newSenders makes a worker's sender of every channel.
func (m *mailerImpl) newSenders() (map[string]ChannelSender, error) {
	senders := make(map[string]ChannelSender, len(m.channels))
	for channel, newSender := range m.channels {
		sender, err := newSender()
		if err != nil {
			return nil, fmt.Errorf("%s sender: %w", channel, err)
		}
		senders[channel] = sender
	}
	return senders, nil
}

// unknownChannel is the failure of a row whose channel this process has no
// sender for: an image with more channels queued it, and this one, rolled
// back to, cannot send it. Retrying would not change that.
func unknownChannel(channel string) error {
	return permanentError{fmt.Errorf("no sender for channel %q", channel)}
}

// emailSender is a worker's e-mail channel: its own SMTP clients, each send
// a connection of its own, bounded by the send budget from its dial. Which
// mailbox a mail goes out from, and how, is its sender lane's (lanes.go).
type emailSender struct {
	client *mail.Client
	config SMTPConfig
	logger *zerolog.Logger
	// db and tracking put a tracked row's pixel and click addresses into its
	// mail (outgoingHTML, tracking.go); without db no mail is tracked.
	db       *database.Store
	tracking Tracking

	// lanes are the sender lanes; templateKey reads a send's template, which
	// picks a mail's lane, only while a lane is on.
	lanes       SendLanes
	templateKey templateKeyOf
	// xoauth2 is the worker's SMTP client for the lanes sent with XOAUTH2:
	// its username and token are set before each send. nil while none is.
	xoauth2        *mail.Client
	exchangeTokens tokenSource
	// graphTokens, graphURL and graphClient are the lanes sent with Graph.
	graphTokens tokenSource
	graphURL    string
	graphClient *http.Client
	sendBudget  time.Duration
}

func (m *mailerImpl) newEmailSender() (ChannelSender, error) {
	client, err := m.newSMTPClient()
	if err != nil {
		return nil, err
	}
	sender := &emailSender{
		client: client, config: m.smtpConfig, logger: m.logger, db: m.db, tracking: m.tracking,
		lanes: m.lanes, templateKey: m.templateKey,
		exchangeTokens: m.exchangeTokens, graphTokens: m.graphTokens, graphURL: m.graphURL, graphClient: m.graphClient,
		sendBudget: m.sendBudget,
	}
	if m.lanes.uses(TransportXOAUTH2) {
		if sender.xoauth2, err = m.newXOAUTH2Client(); err != nil {
			return nil, err
		}
	}
	return sender, nil
}

// smtpOptions are the options every SMTP client of a worker has: STARTTLS
// required, each send bounded by sendBudget from its dial.
func (m *mailerImpl) smtpOptions() []mail.Option {
	options := []mail.Option{
		mail.WithDialContextFunc(dialWithin(m.sendBudget)),
		mail.WithPort(m.smtpConfig.Port),
		mail.WithTLSPolicy(mail.TLSMandatory),
		// go-mail otherwise says NOOP before the mail and again after the
		// relay took it (before RSET): one more round trip that can only
		// fail a send that has gone through.
		mail.WithoutNoop(),
	}
	if m.requestDSN {
		// RET=HDRS NOTIFY=FAILURE, only when the relay's EHLO offers DSN
		// (go-mail checks): a remote server returns the headers, not the
		// mail, and reports a failure only. Never mail.WithDSN(): its
		// NOTIFY=SUCCESS would bring a report of every delivery.
		options = append(options,
			mail.WithDSNMailReturnType(mail.DSNMailReturnHeadersOnly),
			mail.WithDSNRcptNotifyType(mail.DSNRcptNotifyFailure))
	}
	return options
}

// newSMTPClient is a worker's SMTP client of the off path: AUTH LOGIN (or
// PLAIN) as SMTP_USER.
func (m *mailerImpl) newSMTPClient() (*mail.Client, error) {
	authType := mail.SMTPAuthLogin
	if m.smtpConfig.Plain {
		authType = mail.SMTPAuthPlain
	}
	options := append(m.smtpOptions(),
		mail.WithSMTPAuth(authType),
		mail.WithUsername(m.smtpConfig.User),
		mail.WithPassword(m.smtpConfig.Password),
	)
	return mail.NewClient(m.smtpConfig.Host, append(options, m.clientOptions...)...)
}

// newXOAUTH2Client is a worker's SMTP client of the XOAUTH2 lanes, to the
// same relay: AUTH XOAUTH2, always, after the tests' options too.
func (m *mailerImpl) newXOAUTH2Client() (*mail.Client, error) {
	options := append(m.smtpOptions(), m.clientOptions...)
	return mail.NewClient(m.smtpConfig.Host, append(options, mail.WithSMTPAuth(mail.SMTPAuthXOAUTH2))...)
}

// Send sends job from its lane's mailbox the way the lane says; with every
// lane off, as SkyMail always has, without reading the send's template.
func (s *emailSender) Send(ctx context.Context, job database.MailQueue) error {
	lane, config, err := s.laneOf(ctx, job)
	if err != nil {
		return err
	}
	if job, err = s.withUnsubscribeFooter(ctx, lane, job); err != nil {
		return err
	}
	unsubscribe := s.listUnsubscribe(lane, config, job)
	switch config.Transport {
	case TransportGraph:
		if s.graphTokens == nil || config.From == nil {
			return fmt.Errorf("send failed: the %s lane is graph without the application's credentials", lane)
		}
		if unsubscribe != "" {
			return s.sendGraphMIME(ctx, job, lane, config, unsubscribe)
		}
		return s.sendGraph(ctx, job, lane, config)
	case TransportXOAUTH2:
		if s.exchangeTokens == nil || s.xoauth2 == nil || config.From == nil {
			return fmt.Errorf("send failed: the %s lane is xoauth2 without the application's credentials", lane)
		}
		return s.sendXOAUTH2(ctx, job, lane, config, unsubscribe)
	default:
		msg, err := s.message(ctx, job, s.config.FromEmail, false)
		if err != nil {
			return err
		}
		return s.sendSMTP(ctx, job, s.client, msg)
	}
}

// laneOf is job's sender lane and its configuration. The send's template is
// read only while a lane is on; failing to read it is tried again later.
func (s *emailSender) laneOf(ctx context.Context, job database.MailQueue) (Lane, LaneConfig, error) {
	if !s.lanes.anyOn() {
		return "", LaneConfig{Transport: TransportOff}, nil
	}
	lane, err := s.streamOf(ctx, job)
	if err != nil {
		return "", LaneConfig{}, err
	}
	return lane, s.lanes.Lane(lane), nil
}

// streamOf is job's sender lane: its send's template and purpose read
// (LaneOf; the purpose only while SKYMAIL_MAIL_PURPOSE is on).
func (s *emailSender) streamOf(ctx context.Context, job database.MailQueue) (Lane, error) {
	send, err := s.templateKey(ctx, job.TaskID)
	if err != nil {
		return "", fmt.Errorf("read the send's template for its sender lane: %w", err)
	}
	return LaneOf(s.lanes.purposeOf(send.Purpose), send.Key, send.System, Priority(job.Priority)), nil
}

// message is job's mail from from. queueHeader adds X-SkyMail-Queue-Id, which
// the lanes write (graph.go); the off path writes the mail as it always has,
// with SMTP_REPLY_TO's Reply-To when it is set.
func (s *emailSender) message(ctx context.Context, job database.MailQueue, from string, withQueueHeader bool) (*mail.Msg, error) {
	msg := mail.NewMsg()
	if err := msg.From(from); err != nil {
		return nil, permanentError{fmt.Errorf("invalid from email: %w", err)}
	}
	if err := msg.To(fmt.Sprintf("%s <%s>", job.RecipientFullName, job.RecipientEmail)); err != nil {
		// Only the class: go-mail's error quotes the name and the address it
		// could not parse, and net/mail's may echo part of them, while the
		// row's error is logged and shown on the send's page.
		return nil, permanentError{errors.New(invalidRecipientError)}
	}
	if s.lanes.ReplyTo != nil {
		if err := msg.ReplyTo(s.lanes.ReplyTo.String()); err != nil {
			return nil, permanentError{fmt.Errorf("invalid reply-to: %w", err)}
		}
	}

	msg.SetGenHeader(mail.HeaderMessageID, fmt.Sprintf("<%s@%s>", job.ID.String(), s.config.FQDN))
	if withQueueHeader {
		msg.SetGenHeader(mail.Header(dsn.QueueIDHeader), queueHeader(job, s.config.FQDN))
	}

	msg.Subject(job.Subject)
	msg.SetBodyString(mail.TypeTextPlain, job.Body)

	if job.BodyHtml != nil {
		msg.AddAlternativeString(mail.TypeTextHTML, s.outgoingHTML(ctx, job))
	}
	return msg, nil
}

// sendXOAUTH2 sends job from lane's mailbox over SMTP, AUTH XOAUTH2 with an
// Exchange Online token for the application, with List-Unsubscribe when
// unsubscribe is a link. The token is dropped when the connection or the AUTH
// fails, so the next try asks for a new one.
func (s *emailSender) sendXOAUTH2(ctx context.Context, job database.MailQueue, lane Lane, config LaneConfig, unsubscribe string) error {
	msg, err := s.message(ctx, job, config.From.String(), true)
	if err != nil {
		return err
	}
	if unsubscribe != "" {
		setListUnsubscribe(msg, unsubscribe)
	}
	// The token's wait is bounded by the send budget, as Graph's is: a slow
	// Entra must not hold a row past its lease.
	waiting, cancel := context.WithTimeout(ctx, s.sendBudget)
	token, err := s.exchangeTokens.Token(waiting)
	cancel()
	if err != nil {
		return fmt.Errorf("send failed: exchange token: %w", err)
	}
	s.xoauth2.SetUsername(config.From.Address)
	s.xoauth2.SetPassword(token)
	err = s.sendSMTP(ctx, job, s.xoauth2, msg)
	s.xoauth2.SetPassword("")
	var dial dialError
	if errors.As(err, &dial) {
		s.exchangeTokens.Invalidate(token)
	}
	return err
}

// dialError is a send that failed before the mail: the connection, STARTTLS
// or AUTH.
type dialError struct{ err error }

func (e dialError) Error() string { return "dial failed: " + e.err.Error() }
func (e dialError) Unwrap() error { return e.err }

// sendSMTP sends msg over client, a connection of its own.
func (s *emailSender) sendSMTP(ctx context.Context, job database.MailQueue, client *mail.Client, msg *mail.Msg) error {
	// The send's own context, ended when the send returns: the connection
	// is closed when it ends (withSend), on ctx's cancellation as on any
	// other way out, a failed dial included.
	send, done := context.WithCancel(ctx)
	defer done()
	smtpClient, err := client.DialToSMTPClientWithContext(withSend(send))
	if err != nil {
		return dialError{err}
	}
	defer func() { _ = client.CloseWithSMTPClient(smtpClient) }()
	if err := client.SendWithSMTPClient(smtpClient, msg); err != nil {
		if !msg.IsDelivered() {
			// A 5xx fails the row at once, anything else is tried again
			// (smtpFailure).
			return smtpFailure(err)
		}
		// The relay took the mail with the 250 after DATA; what failed is
		// the RSET go-mail says after it (the relay hung up, or the
		// shutdown cut the connection). Retrying would mail the person
		// twice.
		s.logger.Warn().Err(err).Str("job_id", job.ID.String()).
			Msg("The relay took the mail; the SMTP exchange after it failed: counted as sent")
	}
	// Nor is a QUIT that fails or is cut off a failed send (go-mail's
	// DialAndSend reports it as one).
	return nil
}
