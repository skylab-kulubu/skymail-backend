package mailer

import (
	"context"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// SetDeliver makes m send a row with deliver instead of SMTP. Call it before
// Start.
func SetDeliver(m Transactional, deliver func(ctx context.Context, job database.MailQueue) error) {
	m.(*mailerImpl).deliver = func(ctx context.Context, _ *mail.Client, job database.MailQueue) error {
		return deliver(ctx, job)
	}
}

// Claimant is the name m writes in the rows it takes (claimed_by).
func Claimant(m Transactional) string { return m.(*mailerImpl).claimant }

// SetSendBudget bounds m's SMTP sends to budget instead of SendBudget. Call it
// before Start.
func SetSendBudget(m Transactional, budget time.Duration) { m.(*mailerImpl).sendBudget = budget }

// DialWithin is the dialler the workers give go-mail.
var DialWithin = dialWithin

// StartWithoutWorkers starts m taking rows for slots workers without running
// any: what the dispatcher takes waits in the channel.
func StartWithoutWorkers(m Transactional, ctx context.Context, slots int) {
	m.(*mailerImpl).start(ctx, slots, 0)
}

// SetClientOptions adds opts to the options m's workers make their SMTP
// client with (after the production ones, so they win). Call it before Start.
func SetClientOptions(m Transactional, opts ...mail.Option) {
	m.(*mailerImpl).clientOptions = append(m.(*mailerImpl).clientOptions, opts...)
}

// SendOnce sends job the way a worker does (its SMTP client, sendEmail),
// without a queue or a database.
func SendOnce(ctx context.Context, m Transactional, job database.MailQueue) error {
	impl := m.(*mailerImpl)
	client, err := impl.newSMTPClient()
	if err != nil {
		return err
	}
	return impl.sendEmail(ctx, client, job)
}
