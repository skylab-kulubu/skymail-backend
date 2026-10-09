package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlt "html/template"
	"sync"
	"sync/atomic"
	textt "text/template"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// A mail carries the same template twice, once as markup and once as text, and
// safeHTML has to mean a different thing in each. The two maps differ only in
// that; everything else belongs in both.
var (
	htmlFuncs = map[string]any{
		"add1": add1,
		// safeHTML lets a template interpolate markup instead of escaping it.
		// html/template escapes every variable by default, which is what we want
		// everywhere except the free-form template, whose body is rich text the
		// sender wrote. That body is sanitised against an allowlist before it is
		// ever stored, so what reaches here is already narrowed markup.
		"safeHTML": func(v any) htmlt.HTML {
			return htmlt.HTML(sanitizeEmailHTML(textValue(v)))
		},
	}

	textFuncs = map[string]any{
		"add1": add1,
		// The same body, for the plain-text alternative. Printing the markup
		// there would show the recipient `<p>…</p>`, so it is turned into text
		// the same shape html-to-text gives the rest of the mail.
		"safeHTML": func(v any) string {
			return plainTextFromHTML(sanitizeEmailHTML(textValue(v)))
		},
	}
)

func add1(i int) int { return i + 1 }

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
	// Start puts back to pending the processing rows whose lease has run out,
	// keeps doing so while ctx lives, and, unless the sender is paused, starts
	// the dispatcher and workerCount workers.
	Start(ctx context.Context, workerCount int)
	// SenderPaused says MAIL_SENDER=paused holds this process's sender back:
	// mail is queued but none is sent.
	SenderPaused() bool
	Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error)
	EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error)
	EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error)
}

// Queue is the send path inside a caller's transaction: the same three ways
// to queue a send as Mailer's, written through that transaction, so the send
// and its queue rows exist only if it commits, and nothing is dispatched
// before it has. The caller wakes the dispatcher once it has committed.
type Queue interface {
	Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error)
	EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error)
	EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error)
}

// Transactional is a Mailer that can also queue a send in a caller's
// transaction. Mailer's own methods write through the pool, statement by
// statement, and wake the dispatcher, as they always have.
type Transactional interface {
	Mailer
	// Queue writes sends through q, a transaction's queries.
	Queue(q *database.Queries) Queue
	// Wake asks the dispatcher to look at the queue now. Call it after the
	// transaction a Queue wrote in commits; the dispatcher's tick finds the
	// rows anyway, only later.
	Wake()
	// Stop stops sending for shutdown (docs/health-and-shutdown.md): the
	// dispatcher takes no more rows; the rows it took that no worker has
	// begun go back to pending at once, so another process sends them
	// without waiting out the lease; the sends in progress finish and record
	// their outcome; the reaper stops. It returns once all that is done, or
	// with ctx's error when ctx ends first: a send still in progress then
	// keeps its row until the lease runs out, and ending Start's context
	// cuts it off without counting a failed attempt. Queueing still works.
	Stop(ctx context.Context) error
}

type mailerImpl struct {
	db         *database.Store
	jobs       chan database.MailQueue
	nudge      chan struct{}
	logger     *zerolog.Logger
	smtpConfig SMTPConfig
	sender     Sender

	// claimant names this process in the rows it takes (claimed_by); lease is
	// how long a taken row stays its own.
	claimant string
	lease    time.Duration
	// workers is how many rows this process sends at once, and taken how many
	// rows it holds now, in the channel or with a worker. The dispatcher takes
	// only workers - taken rows, so no taken row waits in this process.
	workers int
	taken   atomic.Int32
	// sendBudget bounds one SMTP send from dial to QUIT (SendBudget).
	sendBudget time.Duration
	// deliver sends one row; tests replace it.
	deliver func(ctx context.Context, client *mail.Client, job database.MailQueue) error

	// stop closes when Stop is called; stopOnce closes it. dispatching,
	// sending and reaping are the dispatcher, the workers and the reaper,
	// which Stop waits for.
	stop        chan struct{}
	stopOnce    sync.Once
	dispatching sync.WaitGroup
	sending     sync.WaitGroup
	reaping     sync.WaitGroup
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

// NewMailer makes the mailer. sender is MAIL_SENDER (SenderFromEnv); anything
// but SenderPaused sends.
func NewMailer(db *database.Store, smtpConfig SMTPConfig, sender Sender, opts ...Option) Transactional {
	logger := log.With().Str("service", "mailer").Logger()
	if sender != SenderPaused {
		sender = SenderOn
	}

	m := &mailerImpl{
		db:         db,
		nudge:      make(chan struct{}, 1),
		logger:     &logger,
		smtpConfig: smtpConfig,
		sender:     sender,
		claimant:   newClaimant(),
		lease:      DefaultQueueLease,
		sendBudget: SendBudget,
		stop:       make(chan struct{}),
	}
	m.deliver = m.sendEmail
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *mailerImpl) SenderPaused() bool { return m.sender == SenderPaused }

func (m *mailerImpl) Start(ctx context.Context, workerCount int) {
	m.start(ctx, workerCount, workerCount)
}

// start takes rows for slots workers at a time and runs workers of them;
// Start runs one per slot. Tests run none, to hold taken rows in the channel.
func (m *mailerImpl) start(ctx context.Context, slots, workers int) {
	workerCount := slots
	if m.SenderPaused() {
		m.logger.Warn().Str("sender", string(m.sender)).Str("claimant", m.claimant).
			Msg("Mail sender paused by MAIL_SENDER=paused: mail is queued but nothing will be sent until MAIL_SENDER is removed and the service redeployed")
	} else {
		m.logger.Info().Int("workers", workerCount).Str("sender", string(m.sender)).
			Str("claimant", m.claimant).Dur("lease", m.lease).Msg("Starting up")
	}

	// A row still processing past its lease was taken by a process that is
	// gone, or a restored dump holds it so. A row within its lease is left
	// alone: under a start-first deploy the old task is still sending what it
	// took. Paused too an expired row goes back to pending: the erase endpoint
	// deletes a pending row to the person, but waits (202) on a processing
	// one, which no worker would ever finish. Rows a process leaves behind
	// while this one runs are found by the reaper.
	m.reclaimExpired(ctx)
	m.reaping.Add(1)
	go func() {
		defer m.reaping.Done()
		m.startReaper(ctx)
	}()

	if m.SenderPaused() {
		return
	}

	m.workers = workerCount
	m.jobs = make(chan database.MailQueue, workerCount)
	for i := 0; i < workers; i++ {
		m.sending.Add(1)
		go func() {
			defer m.sending.Done()
			m.startWorker(ctx, i)
		}()
	}

	m.dispatching.Add(1)
	go func() {
		defer m.dispatching.Done()
		m.startDispatcher(ctx)
	}()
}

// stopping reports whether Stop was called.
func (m *mailerImpl) stopping() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

func (m *mailerImpl) Stop(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stop) })
	started := time.Now()

	// The dispatcher finishes the take it is in, if any, and puts what it
	// took in the channel; after it nothing more is taken.
	if err := waitGroup(ctx, &m.dispatching); err != nil {
		m.logger.Warn().Err(err).Msg("Stop: the dispatcher did not stop in time")
		return err
	}
	released := m.releaseWaiting(ctx)
	if err := waitGroup(ctx, &m.sending); err != nil {
		m.logger.Warn().Err(err).Int("released", released).Int32("in_progress", m.taken.Load()).
			Dur("lease", m.lease).
			Msg("Stop: sends still in progress at the deadline keep their rows until the lease runs out")
		return err
	}
	// A worker that stopped between taking a row and seeing the stop gave
	// it back itself; one that saw the stop first left the row here.
	released += m.releaseWaiting(ctx)
	if err := waitGroup(ctx, &m.reaping); err != nil {
		return err
	}
	m.logger.Info().Int("released", released).Dur("took", time.Since(started).Round(time.Millisecond)).
		Msg("Stopped: rows taken and not begun are pending again; sends in progress finished")
	return nil
}

// waitGroup waits for group until ctx ends.
func waitGroup(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseWaiting gives back every row waiting in the workers' channel and
// returns how many it gave back.
func (m *mailerImpl) releaseWaiting(ctx context.Context) int {
	released := 0
	for {
		select {
		case job := <-m.jobs:
			if m.release(ctx, m.logger, job) {
				released++
			}
		default:
			return released
		}
	}
}

// release gives back a row this process took and has not begun to send: it
// is pending again, unclaimed, with no attempt counted.
func (m *mailerImpl) release(ctx context.Context, logger *zerolog.Logger, job database.MailQueue) bool {
	defer m.taken.Add(-1)
	n, err := m.db.ReleaseMailQueueItem(ctx, database.ReleaseMailQueueItemParams{
		ID:        job.ID,
		ClaimedBy: m.claimant,
		ClaimedAt: claimedAt(job),
	})
	if err != nil {
		logger.Err(err).Str("job_id", job.ID.String()).Dur("lease", m.lease).
			Msg("Failed to give back a mail queue item at shutdown: it is sent after its lease runs out")
		return false
	}
	if n == 0 {
		m.lostLease(logger, job, "released")
		return false
	}
	return true
}

// reapEvery is how often a process looks for rows whose lease has run out: a
// fifth of the lease, so a dead process's rows wait at most a lease and a fifth.
func (m *mailerImpl) reapEvery() time.Duration {
	if every := m.lease / 5; every >= time.Second {
		return every
	}
	return time.Second
}

func (m *mailerImpl) startReaper(ctx context.Context) {
	ticker := time.NewTicker(m.reapEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			m.reclaimExpired(ctx)
		}
	}
}

// reclaimExpired starts the lease of every processing row that has none (taken
// by an image from before the lease) and puts back to pending every processing
// row whose lease has run out.
func (m *mailerImpl) reclaimExpired(ctx context.Context) {
	stamped, err := m.db.StartLeaseOnUnclaimedJobs(ctx)
	if err != nil {
		m.logger.Err(err).Msg("Failed to start the lease of unclaimed jobs")
	} else if stamped > 0 {
		m.logger.Info().Int64("count", stamped).Dur("lease", m.lease).
			Msg("Processing rows without a claim: their lease starts now")
	}

	reset, err := m.db.ResetDeadJobs(ctx, int(m.lease.Seconds()))
	if err != nil {
		m.logger.Err(err).Msg("Failed to reset dead jobs")
		return
	}
	if reset > 0 {
		m.logger.Warn().Int64("count", reset).Dur("lease", m.lease).
			Msg("Put processing rows whose lease ran out back to pending")
		m.wake()
	}
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

// queue writes sends through q: the pool's queries, or a transaction's.
type queue struct {
	q      *database.Queries
	logger *zerolog.Logger
}

func (m *mailerImpl) Queue(q *database.Queries) Queue {
	return &queue{q: q, logger: m.logger}
}

func (m *mailerImpl) Wake() { m.wake() }

// pooled is a send written in a transaction of its own, which wakes the
// dispatcher once rows are queued.
func (m *mailerImpl) pooled() *queue {
	return &queue{q: m.db.Queries, logger: m.logger}
}

func (m *mailerImpl) Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error) {
	id, queued, err := m.pooled().enqueue(ctx, arg)
	m.wakeIf(queued)
	return id, err
}

func (m *mailerImpl) EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	id, queued, err := m.pooled().enqueueSingle(ctx, arg)
	m.wakeIf(queued)
	return id, err
}

func (m *mailerImpl) EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error) {
	id, queued, err := m.pooled().enqueueWithRecipients(ctx, params)
	m.wakeIf(queued)
	return id, err
}

func (m *mailerImpl) wakeIf(queued bool) {
	if queued {
		m.wake()
	}
}

func (q *queue) Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error) {
	id, _, err := q.enqueue(ctx, arg)
	return id, err
}

func (q *queue) EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	id, _, err := q.enqueueSingle(ctx, arg)
	return id, err
}

func (q *queue) EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error) {
	id, _, err := q.enqueueWithRecipients(ctx, params)
	return id, err
}

// The three ways to queue a send return the send and whether queue rows were
// written, which is when the dispatcher has something to do.

func (q *queue) enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, bool, error) {
	rows, err := q.q.CreateMailTask(ctx, arg)
	if err != nil {
		q.logger.Err(err).Msg("Failed to enqueue mail task")
		return uuid.Nil, false, err
	}

	if len(rows) == 0 {
		q.logger.Warn().Msg("No mail queue items were created")
		return uuid.Nil, false, nil
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

	queued, err := q.renderAndQueue(ctx, commonRows)
	return rows[0].TaskID, queued, err
}

func (q *queue) enqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, bool, error) {
	row, err := q.q.CreateSingleMailTask(ctx, arg)
	if err != nil {
		q.logger.Err(err).Msg("Failed to enqueue single mail task")
		return uuid.Nil, false, err
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

	queued, err := q.renderAndQueue(ctx, commonRows)
	return row.TaskID, queued, err
}

func (q *queue) enqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, bool, error) {
	task, err := q.q.InsertMailTask(ctx, database.InsertMailTaskParams{
		SentBy:        params.SentBy,
		TemplateID:    &params.TemplateID,
		MailListID:    params.MailListID,
		BodyVariables: params.BodyVariables,
	})
	if err != nil {
		return uuid.Nil, false, err
	}

	tmpl, err := q.q.GetTemplateById(ctx, *task.TemplateID)
	if err != nil {
		return uuid.Nil, false, err
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

	queued, err := q.renderAndQueue(ctx, rows)
	return task.ID, queued, err
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
	subjectTemplate, err := textt.New("subject").Funcs(textFuncs).Parse(subject)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartSubject, Err: err}
	}
	textTemplate, err := textt.New("text").Funcs(textFuncs).Parse(plainText)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartPlainText, Err: err}
	}
	htmlTemplate, err := htmlt.New("html").Funcs(htmlFuncs).Parse(html)
	if err != nil {
		return mailTemplates{}, &ParseError{Part: PartHTML, Err: err}
	}
	return mailTemplates{subject: subjectTemplate, text: textTemplate, html: htmlTemplate}, nil
}

// Rendered is one recipient's mail as a send queues it.
type Rendered struct {
	Subject   string `json:"subject"`
	PlainText string `json:"plain_text"`
	HTML      string `json:"html"`
}

// Render renders a Mail template's subject, plain text and HTML for one
// recipient exactly as a send renders what it queues: the send's variables —
// a JSON object, as a mail task keeps them — with the recipient's Email and
// FullName over them. A part that does not parse is a *ParseError.
func Render(subject, plainText, html string, variables []byte, recipient RecipientInfo) (Rendered, error) {
	parsed, err := parseMailTemplates(subject, plainText, html)
	if err != nil {
		return Rendered{}, err
	}
	var vars map[string]interface{}
	if err := json.Unmarshal(variables, &vars); err != nil {
		return Rendered{}, fmt.Errorf("invalid json variables: %w", err)
	}
	return parsed.render(vars, recipient)
}

// render executes the three parts for one recipient. It is the one place a
// send and a preview render, so a preview is what the send would queue.
func (t mailTemplates) render(vars map[string]interface{}, recipient RecipientInfo) (Rendered, error) {
	data := make(map[string]interface{}, len(vars)+2)
	for k, v := range vars {
		data[k] = v
	}
	data["Email"] = recipient.Email
	data["FullName"] = recipient.FullName

	var subject, text, html bytes.Buffer
	if err := t.subject.Execute(&subject, data); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartSubject, err)
	}
	if err := t.text.Execute(&text, data); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartPlainText, err)
	}
	if err := t.html.Execute(&html, data); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartHTML, err)
	}
	return Rendered{Subject: subject.String(), PlainText: text.String(), HTML: html.String()}, nil
}

// renderAndQueue renders each row's mail and writes the queue rows, saying
// whether it wrote any.
func (q *queue) renderAndQueue(ctx context.Context, rows []commonMailRow) (bool, error) {
	if len(rows) == 0 {
		return false, nil
	}

	var taskVars map[string]interface{}
	if err := json.Unmarshal(rows[0].BodyVariables, &taskVars); err != nil {
		return false, fmt.Errorf("invalid json variables: %w", err)
	}

	parsed, err := parseMailTemplates(rows[0].TemplateSubject, rows[0].PlainTextContent, rows[0].HtmlContent)
	if err != nil {
		return false, err
	}

	queueItems := make([]database.CreateMailQueueItemsParams, len(rows))
	for i, row := range rows {
		rendered, err := parsed.render(taskVars, RecipientInfo{FullName: row.RecipientFullName, Email: row.RecipientEmail})
		if err != nil {
			q.logger.Err(err).Msg("Failed to render template")
			continue
		}

		htmlRes := rendered.HTML

		queueItems[i] = database.CreateMailQueueItemsParams{
			TaskID:            row.TaskID,
			RecipientFullName: row.RecipientFullName,
			RecipientEmail:    row.RecipientEmail,
			Subject:           rendered.Subject,
			Body:              rendered.PlainText,
			BodyHtml:          &htmlRes,
		}
	}

	if _, err = q.q.CreateMailQueueItems(ctx, queueItems); err != nil {
		return false, err
	}
	return true, nil
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

		case <-m.stop:
			logger.Info().Msg("Stopped: taking no more rows")
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

// drainQueue takes as many due rows as this process has idle workers. A
// worker that finishes a row wakes the dispatcher, which takes the next.
func (m *mailerImpl) drainQueue(ctx context.Context, logger *zerolog.Logger) bool {
	// The tick or a nudge may win the select over a stop already made.
	if m.stopping() {
		return false
	}
	idle := m.workers - int(m.taken.Load())
	if idle <= 0 {
		return true
	}

	items, err := m.db.ProcessQueueItems(ctx, database.ProcessQueueItemsParams{
		ClaimedBy: m.claimant,
		MaxRows:   idle,
	})
	if err != nil {
		logger.Err(err).Msg("Failed to process queue items")
	}

	if len(items) > 0 {
		m.taken.Add(int32(len(items)))
		logger.Debug().Int("count", len(items)).Msg("Pulled pending jobs from database")
	}

	// The channel holds a row per worker and no more rows are taken than
	// workers are idle, so this never waits on a worker; at a stop the rows
	// are in the channel, where Stop gives them back.
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
		mail.WithDialContextFunc(dialWithin(m.sendBudget)),
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
		case <-m.stop:
			logger.Info().Msg("Stopped")
			return
		case job := <-m.jobs:
			if m.stopping() {
				// The row and the stop came together: give the row back
				// rather than begin it.
				m.release(ctx, &logger, job)
				continue
			}
			logger.Debug().Str("job_id", job.ID.String()).Msg("Worker picked up job")
			m.send(ctx, client, &logger, job)

			// This worker is idle again: the dispatcher may take a row for it.
			m.taken.Add(-1)
			m.wake()
		}
	}
}

// outcomeTimeout bounds writing a send's outcome.
const outcomeTimeout = 5 * time.Second

// send sends job and records the outcome. The outcome is written even when
// ctx ended as the send finished: a mail the relay took and the row not
// saying so would go out again after the lease. A send cut off because ctx
// ended (the process going away) is no failed attempt: the row keeps its
// claim and is put back when the lease runs out.
func (m *mailerImpl) send(ctx context.Context, client *mail.Client, logger *zerolog.Logger, job database.MailQueue) {
	err := m.deliver(ctx, client, job)
	if err != nil && ctx.Err() != nil {
		logger.Warn().Err(err).Str("job_id", job.ID.String()).Dur("lease", m.lease).
			Msg("Send cut off by shutdown: no attempt counted; the row is sent again after its lease runs out")
		return
	}
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
	defer cancel()
	if err != nil {
		m.recordSendFailure(record, logger, job, err)
		return
	}
	logger.Debug().Str("job_id", job.ID.String()).Msg("Sent email")

	n, err := m.db.SetMailQueueItemSent(record, database.SetMailQueueItemSentParams{
		ID:        job.ID,
		ClaimedBy: m.claimant,
		ClaimedAt: claimedAt(job),
	})
	if err != nil {
		logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to set mail queue item")
	} else if n == 0 {
		m.lostLease(logger, job, "sent")
	}
}

// claimedAt is when this claim of job began, which with the claimant fences
// its outcome: a later claim of the row, by this process too, has another.
func claimedAt(job database.MailQueue) time.Time {
	if job.ClaimedAt == nil {
		return time.Time{}
	}
	return *job.ClaimedAt
}

// lostLease logs an outcome this process could not record: the row's lease
// ran out and it was put back to pending, or another process took it since.
// That process sends it and records the outcome.
func (m *mailerImpl) lostLease(logger *zerolog.Logger, job database.MailQueue, outcome string) {
	logger.Warn().Str("job_id", job.ID.String()).Str("outcome", outcome).
		Str("claimant", m.claimant).Dur("lease", m.lease).
		Msg("Lease on the mail queue item ran out: outcome not recorded")
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

		n, err := m.db.SetMailQueueItemFailed(ctx, database.SetMailQueueItemFailedParams{
			ID:        job.ID,
			ClaimedBy: m.claimant,
			ClaimedAt: claimedAt(job),
			Error:     &reason,
		})
		if err != nil {
			logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to set mail queue item")
		} else if n == 0 {
			m.lostLease(logger, job, "failed")
		}
		return
	}

	delay := retryDelay(job.Attempts)
	attempts, err := m.db.RescheduleMailQueueItem(ctx, database.RescheduleMailQueueItemParams{
		ID:           job.ID,
		ClaimedBy:    m.claimant,
		ClaimedAt:    claimedAt(job),
		Error:        &reason,
		DelaySeconds: int(delay.Seconds()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		m.lostLease(logger, job, "retry")
		return
	}
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
