package mailer

import (
	"context"

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
