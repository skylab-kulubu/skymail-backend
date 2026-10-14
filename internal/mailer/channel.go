package mailer

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// A queue row is one delivery to one person over one channel
// (mail_queue.channel). The engine (the lanes, the lease and its fencing, the
// retries, the send budget) does not care how a row goes out: a worker hands
// each row it takes to the sender of the row's channel. E-mail is the only
// channel yet; recipient_email is its address.
const ChannelEmail = "email"

// ChannelSender sends the rows of one channel. Each worker makes its own of
// every channel at startup and sends one row at a time with it, so what a
// channel keeps between sends (the SMTP client) is the worker's alone.
//
// Send's error is how the send went: nil, sent; a permanentError, failed for
// good at once (retrying cannot fix it); any other, tried again later, up to
// maxSendAttempts. An error while ctx has ended is the process going away,
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

// emailSender is a worker's e-mail channel: its own SMTP client, each send a
// connection of its own, bounded by the send budget from its dial.
type emailSender struct {
	client *mail.Client
	config SMTPConfig
	logger *zerolog.Logger
}

func (m *mailerImpl) newEmailSender() (ChannelSender, error) {
	client, err := m.newSMTPClient()
	if err != nil {
		return nil, err
	}
	return &emailSender{client: client, config: m.smtpConfig, logger: m.logger}, nil
}

// newSMTPClient is a worker's SMTP client: STARTTLS required, each send
// bounded by sendBudget from its dial.
func (m *mailerImpl) newSMTPClient() (*mail.Client, error) {
	authType := mail.SMTPAuthLogin
	if m.smtpConfig.Plain {
		authType = mail.SMTPAuthPlain
	}
	options := append([]mail.Option{
		mail.WithDialContextFunc(dialWithin(m.sendBudget)),
		mail.WithPort(m.smtpConfig.Port),
		mail.WithSMTPAuth(authType),
		mail.WithUsername(m.smtpConfig.User),
		mail.WithPassword(m.smtpConfig.Password),
		mail.WithTLSPolicy(mail.TLSMandatory),
		// go-mail otherwise says NOOP before the mail and again after the
		// relay took it (before RSET): one more round trip that can only
		// fail a send that has gone through.
		mail.WithoutNoop(),
	}, m.clientOptions...)
	return mail.NewClient(m.smtpConfig.Host, options...)
}

func (s *emailSender) Send(ctx context.Context, job database.MailQueue) error {
	msg := mail.NewMsg()
	if err := msg.From(s.config.FromEmail); err != nil {
		return permanentError{fmt.Errorf("invalid from email: %w", err)}
	}
	if err := msg.To(fmt.Sprintf("%s <%s>", job.RecipientFullName, job.RecipientEmail)); err != nil {
		return permanentError{fmt.Errorf("invalid recipient: %w", err)}
	}

	msg.SetGenHeader(mail.HeaderMessageID, fmt.Sprintf("<%s@%s>", job.ID.String(), s.config.FQDN))

	msg.Subject(job.Subject)
	msg.SetBodyString(mail.TypeTextPlain, job.Body)

	if job.BodyHtml != nil {
		msg.AddAlternativeString(mail.TypeTextHTML, *job.BodyHtml)
	}

	// The send's own context, ended when the send returns: the connection
	// is closed when it ends (withSend), on ctx's cancellation as on any
	// other way out, a failed dial included.
	send, done := context.WithCancel(ctx)
	defer done()
	smtpClient, err := s.client.DialToSMTPClientWithContext(withSend(send))
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer func() { _ = s.client.CloseWithSMTPClient(smtpClient) }()
	if err := s.client.SendWithSMTPClient(smtpClient, msg); err != nil {
		if !msg.IsDelivered() {
			return fmt.Errorf("send failed: %w", err)
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
