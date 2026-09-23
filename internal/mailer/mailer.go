package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

var mailFuncs = map[string]any{
	"add1": func(i int) int { return i + 1 },
	// safeHTML lets a template interpolate markup instead of escaping it.
	// html/template escapes every variable by default, which is what we want
	// everywhere except the free-form template, whose body is rich text the
	// sender wrote. That body is sanitised against an allowlist before it is
	// ever stored, so what reaches here is already narrowed markup.
	"safeHTML": func(v any) htmlt.HTML {
		switch value := v.(type) {
		case nil:
			return ""
		case htmlt.HTML:
			return value
		case string:
			return htmlt.HTML(sanitizeEmailHTML(value))
		default:
			return htmlt.HTML(sanitizeEmailHTML(fmt.Sprint(value)))
		}
	},
}

const (
	// A transient SMTP failure (greylisting, a dropped connection, a rate limit)
	// should not lose the mail. Five tries over ~8 minutes covers the usual
	// hiccup; past that the address or the relay is the problem, not the moment.
	maxSendAttempts = 5
	baseRetryDelay  = 30 * time.Second
)

// retryDelay backs off 30s, 60s, 2m, 4m.
func retryDelay(attempts int) time.Duration {
	delay := baseRetryDelay << attempts
	if max := 10 * time.Minute; delay > max {
		delay = max
	}
	return delay
}

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
	nudge      chan struct{}
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
		nudge:      make(chan struct{}, 1),
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

// mailTemplates holds one template's three parsed parts.
type mailTemplates struct {
	subject *textt.Template
	text    *textt.Template
	html    *htmlt.Template
}

// MailPart is one of the three parts of a Mail template a send parses.
type MailPart string

const (
	PartSubject   MailPart = "subject"
	PartPlainText MailPart = "plain text"
	PartHTML      MailPart = "html"
)

// ParseError is a part of a Mail template that does not parse.
type ParseError struct {
	Part MailPart
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("invalid %s template: %v", e.Part, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// CheckTemplate parses a Mail template's subject, plain text and HTML the way
// a send parses them, and returns a *ParseError for the first part that does
// not parse: a template stored like that would fail every send of it.
func CheckTemplate(subject, plainText, html string) error {
	_, err := parseMailTemplates(subject, plainText, html)
	return err
}

// parseMailTemplates parses all three parts or returns the first failure,
// naming the part that failed. Returning is the whole point: a Parse error
// hands back a nil template, so a part whose error is merely logged reaches
// Execute and dereferences nil. Keeping the three together means no caller can
// reintroduce that by handling one of them differently.
func parseMailTemplates(subject, plainText, html string) (mailTemplates, error) {
	subjectTemplate, err := parseSubject(subject)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartSubject, Err: err}
	}
	textTemplate, err := textt.New("text").Funcs(mailFuncs).Parse(plainText)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartPlainText, Err: err}
	}
	htmlTemplate, err := htmlt.New("html").Funcs(mailFuncs).Parse(html)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartHTML, Err: err}
	}
	return mailTemplates{subject: subjectTemplate, text: textTemplate, html: htmlTemplate}, nil
}

func parseSubject(subject string) (*textt.Template, error) {
	return textt.New("subject").Funcs(mailFuncs).Parse(subject)
}

// ParseSubject reports whether a subject parses the way the mailer parses it
// before every send, and the parser's error when it does not.
func ParseSubject(subject string) error {
	_, err := parseSubject(subject)
	return err
}

func (m *mailerImpl) renderAndQueue(ctx context.Context, rows []commonMailRow) error {
	if len(rows) == 0 {
		return nil
	}

	var taskVars map[string]interface{}
	if err := json.Unmarshal(rows[0].BodyVariables, &taskVars); err != nil {
		return fmt.Errorf("invalid json variables: %w", err)
	}

	parsed, err := parseMailTemplates(rows[0].TemplateSubject, rows[0].PlainTextContent, rows[0].HtmlContent)
	if err != nil {
		return err
	}
	subjectTemplate, textTemplate, htmlTemplate := parsed.subject, parsed.text, parsed.html

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

	if _, err = m.db.CreateMailQueueItems(ctx, queueItems); err != nil {
		return err
	}

	m.wake()
	return nil
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
			if !m.drainQueue(ctx, &logger) {
				return
			}

		case <-m.nudge:
			// A single send should not wait out the tick: Keycloak's password
			// reset and verification mails are the ones a person is staring at.
			if !m.drainQueue(ctx, &logger) {
				return
			}
		}
	}
}

func (m *mailerImpl) drainQueue(ctx context.Context, logger *zerolog.Logger) bool {
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
			return false
		}
	}

	return true
}

// wake asks the dispatcher to look at the queue now. It never blocks: a nudge
// already waiting is as good as a second one.
func (m *mailerImpl) wake() {
	select {
	case m.nudge <- struct{}{}:
	default:
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
				m.recordSendFailure(ctx, &logger, job, err)
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

// permanentError marks a failure that retrying cannot fix — a malformed address
// will be just as malformed in four minutes.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func (m *mailerImpl) recordSendFailure(ctx context.Context, logger *zerolog.Logger, job database.MailQueue, sendErr error) {
	reason := sendErr.Error()

	var permanent permanentError
	givingUp := errors.As(sendErr, &permanent) || job.Attempts+1 >= maxSendAttempts

	if givingUp {
		logger.Err(sendErr).
			Str("job_id", job.ID.String()).
			Int("attempts", job.Attempts+1).
			Msg("Giving up on email")

		if err := m.db.SetMailQueueItemFailed(ctx, database.SetMailQueueItemFailedParams{
			ID:    job.ID,
			Error: &reason,
		}); err != nil {
			logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to set mail queue item")
		}
		return
	}

	delay := retryDelay(job.Attempts)
	attempts, err := m.db.RescheduleMailQueueItem(ctx, database.RescheduleMailQueueItemParams{
		ID:           job.ID,
		Error:        &reason,
		DelaySeconds: int(delay.Seconds()),
	})
	if err != nil {
		logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to reschedule mail queue item")
		return
	}

	logger.Warn().Err(sendErr).
		Str("job_id", job.ID.String()).
		Int("attempts", attempts).
		Dur("retry_in", delay).
		Msg("Retrying email")
}

func (m *mailerImpl) sendEmail(ctx context.Context, client *mail.Client, job database.MailQueue) error {
	msg := mail.NewMsg()
	if err := msg.From(m.smtpConfig.FromEmail); err != nil {
		return permanentError{fmt.Errorf("invalid from email: %w", err)}
	}
	if err := msg.To(fmt.Sprintf("%s <%s>", job.RecipientFullName, job.RecipientEmail)); err != nil {
		return permanentError{fmt.Errorf("invalid recipient: %w", err)}
	}

	msg.SetGenHeader(mail.HeaderMessageID, fmt.Sprintf("<%s@%s>", job.ID.String(), m.smtpConfig.FQDN))

	msg.Subject(job.Subject)
	msg.SetBodyString(mail.TypeTextPlain, job.Body)

	if job.BodyHtml != nil {
		msg.AddAlternativeString(mail.TypeTextHTML, *job.BodyHtml)
	}

	return client.DialAndSendWithContext(ctx, msg)
}
