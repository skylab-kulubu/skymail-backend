package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	htmlt "html/template"
	textt "text/template"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

type RecipientInfo struct {
	FullName string
	Email    string
}

type EnqueueWithRecipientsParams struct {
	SentBy        string
	TemplateID    uuid.UUID
	MailListID    *uuid.UUID
	BodyVariables []byte
	Recipients    []RecipientInfo
}

type Mailer interface {
	Start(ctx context.Context, workerCount int)
	Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error)
	EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error)
	EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error)
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
	FQDN      string
	Plain     bool
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

type commonMailRow struct {
	TaskID            uuid.UUID
	BodyVariables     []byte
	TemplateSubject   string
	HtmlContent       string
	PlainTextContent  string
	RecipientFullName string
	RecipientEmail    string
}

func (m *mailerImpl) Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error) {
	rows, err := m.db.CreateMailTask(ctx, arg)
	if err != nil {
		m.logger.Err(err).Msg("Failed to enqueue mail task")
		return uuid.Nil, err
	}

	if len(rows) == 0 {
		m.logger.Warn().Msg("No mail queue items were created")
		return uuid.Nil, nil
	}

	commonRows := make([]commonMailRow, len(rows))
	for i, row := range rows {
		commonRows[i] = commonMailRow{
			TaskID:            row.TaskID,
			BodyVariables:     row.BodyVariables,
			TemplateSubject:   row.TemplateSubject,
			HtmlContent:       row.HtmlContent,
			PlainTextContent:  row.PlainTextContent,
			RecipientFullName: row.RecipientFullName,
			RecipientEmail:    row.RecipientEmail,
		}
	}

	return rows[0].TaskID, m.renderAndQueue(ctx, commonRows)
}

func (m *mailerImpl) EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	row, err := m.db.CreateSingleMailTask(ctx, arg)
	if err != nil {
		m.logger.Err(err).Msg("Failed to enqueue single mail task")
		return uuid.Nil, err
	}

	commonRows := []commonMailRow{
		{
			TaskID:            row.TaskID,
			BodyVariables:     row.BodyVariables,
			TemplateSubject:   row.TemplateSubject,
			HtmlContent:       row.HtmlContent,
			PlainTextContent:  row.PlainTextContent,
			RecipientFullName: row.RecipientFullName,
			RecipientEmail:    row.RecipientEmail,
		},
	}

	return row.TaskID, m.renderAndQueue(ctx, commonRows)
}

func (m *mailerImpl) EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error) {
	task, err := m.db.InsertMailTask(ctx, database.InsertMailTaskParams{
		SentBy:        params.SentBy,
		TemplateID:    &params.TemplateID,
		MailListID:    params.MailListID,
		BodyVariables: params.BodyVariables,
	})
	if err != nil {
		return uuid.Nil, err
	}

	tmpl, err := m.db.GetTemplateById(ctx, *task.TemplateID)
	if err != nil {
		return uuid.Nil, err
	}

	rows := make([]commonMailRow, 0, len(params.Recipients))
	for _, r := range params.Recipients {
		rows = append(rows, commonMailRow{
			TaskID:            task.ID,
			BodyVariables:     params.BodyVariables,
			TemplateSubject:   tmpl.Subject,
			HtmlContent:       tmpl.HtmlContent,
			PlainTextContent:  tmpl.PlainTextContent,
			RecipientFullName: r.FullName,
			RecipientEmail:    r.Email,
		})
	}

	return task.ID, m.renderAndQueue(ctx, rows)
}

func (m *mailerImpl) renderAndQueue(ctx context.Context, rows []commonMailRow) error {
	if len(rows) == 0 {
		return nil
	}

	var taskVars map[string]interface{}
	if err := json.Unmarshal(rows[0].BodyVariables, &taskVars); err != nil {
		return fmt.Errorf("invalid json variables: %w", err)
	}

	subjectTemplate, err := textt.New("subject").Parse(rows[0].TemplateSubject)
	if err != nil {
		m.logger.Err(err).Msg("Failed to parse subject template")
	}
	textTemplate, err := textt.New("text").Parse(rows[0].PlainTextContent)
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}
	htmlTemplate, err := htmlt.New("html").Parse(rows[0].HtmlContent)
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}

	queueItems := make([]database.CreateMailQueueItemsParams, len(rows))
	for i, row := range rows {
		renderData := make(map[string]interface{})
		for k, v := range taskVars {
			renderData[k] = v
		}
		renderData["Email"] = row.RecipientEmail
		renderData["FullName"] = row.RecipientFullName

		var subjectBuf bytes.Buffer
		err = subjectTemplate.Execute(&subjectBuf, renderData)
		if err != nil {
			m.logger.Err(err).Msg("Failed to render subject template")
			continue
		}

		var textBuf bytes.Buffer
		err = textTemplate.Execute(&textBuf, renderData)
		if err != nil {
			m.logger.Err(err).Msg("Failed to render template")
			continue
		}

		var htmlBuf bytes.Buffer
		err = htmlTemplate.Execute(&htmlBuf, renderData)
		if err != nil {
			m.logger.Err(err).Msg("Failed to render template")
			continue
		}

		htmlRes := htmlBuf.String()

		queueItems[i] = database.CreateMailQueueItemsParams{
			TaskID:            row.TaskID,
			RecipientFullName: row.RecipientFullName,
			RecipientEmail:    row.RecipientEmail,
			Subject:           subjectBuf.String(),
			Body:              textBuf.String(),
			BodyHtml:          &htmlRes,
		}
	}

	_, err = m.db.CreateMailQueueItems(ctx, queueItems)
	return err
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

	var authType mail.SMTPAuthType

	if m.smtpConfig.Plain {
		authType = mail.SMTPAuthPlain
	} else {
		authType = mail.SMTPAuthLogin
	}

	client, err := mail.NewClient(m.smtpConfig.Host,
		mail.WithPort(m.smtpConfig.Port),
		mail.WithSMTPAuth(authType),
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

	msg.SetGenHeader(mail.HeaderMessageID, fmt.Sprintf("<%s@%s>", job.ID.String(), m.smtpConfig.FQDN))

	msg.Subject(job.Subject)
	msg.SetBodyString(mail.TypeTextPlain, job.Body)

	if job.BodyHtml != nil {
		msg.AddAlternativeString(mail.TypeTextHTML, *job.BodyHtml)
	}

	return client.DialAndSendWithContext(ctx, msg)
}
