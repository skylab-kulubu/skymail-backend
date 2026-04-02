package mailer

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

type Mailer interface {
	Start(ctx context.Context, workerCount int)
}

type mailerImpl struct {
	db         *database.Store
	jobs       chan database.MailQueue
	logger     *zerolog.Logger
	smtpConfig SMTPConfig
}

type SMTPConfig struct {
	FromEmail string
	Host      string
	Port      int
	User      string
	Password  string
}

func NewMailer(db *database.Store, smtpConfig SMTPConfig) Mailer {
	logger := log.With().Str("service", "mailer").Logger()

	return &mailerImpl{
		db:         db,
		jobs:       make(chan database.MailQueue, 100),
		logger:     &logger,
		smtpConfig: smtpConfig,
	}
}

func (m *mailerImpl) Start(ctx context.Context, workerCount int) {
	m.logger.Info().Int("workers", workerCount).Msg("Starting up")

	err := m.db.ResetDeadJobs(ctx)
	if err != nil {
		m.logger.Err(err).Msg("Failed to reset dead jobs")
	}

	for i := 0; i < workerCount; i++ {
		go m.startWorker(ctx, i)
	}

	go m.startDispatcher(ctx)
}

func (m *mailerImpl) startDispatcher(ctx context.Context) {
	logger := m.logger.With().Str("component", "dispatcher").Logger()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	logger.Info().Msg("Started up")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Shutting down")
			return

		case <-ticker.C:
			items, err := m.db.ProcessQueueItems(ctx)
			if err != nil {
				logger.Err(err).Msg("Failed to process queue items")
			}

			if len(items) > 0 {
				logger.Debug().Int("count", len(items)).Msg("Pulled pending jobs from database")
			}

			for _, item := range items {
				select {
				case m.jobs <- item:
					logger.Debug().Str("job_id", item.ID.String()).Msg("Dispatched job to worker channel")
				case <-ctx.Done():
					logger.Info().Msg("Shutting down")
					return
				}
			}
		}
	}
}

func (m *mailerImpl) startWorker(ctx context.Context, id int) {
	logger := m.logger.With().Str("component", "worker").Int("worker_id", id).Logger()

	client, err := mail.NewClient(m.smtpConfig.Host,
		mail.WithPort(m.smtpConfig.Port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(m.smtpConfig.User),
		mail.WithPassword(m.smtpConfig.Password),
		mail.WithTLSPolicy(mail.TLSMandatory),
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to create SMTP client")
		return
	}

	logger.Info().Msg("Started up")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Shutting down")
			return
		case job := <-m.jobs:
			logger.Debug().Str("job_id", job.ID.String()).Msg("Worker picked up job")

			err := m.sendEmail(ctx, client, job)
			if err != nil {
				logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to send email")
				e := err.Error()

				err := m.db.SetMailQueueItemFailed(ctx, database.SetMailQueueItemFailedParams{
					ID:    job.ID,
					Error: &e,
				})
				if err != nil {
					logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to set mail queue item")
				}
			} else {
				logger.Debug().Str("job_id", job.ID.String()).Msg("Sent email")

				err := m.db.SetMailQueueItemSent(ctx, job.ID)
				if err != nil {
					logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to set mail queue item")
				}
			}
		}
	}
}

func (m *mailerImpl) sendEmail(ctx context.Context, client *mail.Client, job database.MailQueue) error {
	msg := mail.NewMsg()
	if err := msg.From(m.smtpConfig.FromEmail); err != nil {
		return fmt.Errorf("invalid from email: %w", err)
	}
	if err := msg.To(fmt.Sprintf("%s <%s>", job.RecipientFullName, job.RecipientEmail)); err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}
	msg.Subject(job.Subject)
	msg.SetBodyString(mail.TypeTextPlain, job.Body)

	if job.BodyHtml != nil {
		msg.AddAlternativeString(mail.TypeTextHTML, *job.BodyHtml)
	}

	return client.DialAndSendWithContext(ctx, msg)
}
