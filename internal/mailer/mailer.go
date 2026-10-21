package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlt "html/template"
	"net/http"
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

// Priority is a queue row's lane (mail_queue.priority). The dispatcher takes
// the system lane first and gives the bulk lane at most BulkSharePercent of
// its workers and of the send budget (RateBudget), so a send to a list or a
// group never holds back a mail a person is waiting for. A row queued without
// one (an image from before the lanes) is in the system lane.
type Priority int16

const (
	// PriorityBulk is a send to a mailing list or a Keycloak group, an
	// approved send, and a single mail a person sends asking for tracking:
	// an operator's mail, which can wait a moment.
	PriorityBulk Priority = 0
	// PrioritySystem is a single mail (POST /mail_tasks/single): a login
	// code, a certificate, a Forms confirmation, a notification. It is
	// tracked only as a service's mail, with SKYMAIL_TRACKING_SYSTEM on
	// (EnqueueSystemSingle).
	PrioritySystem Priority = 1
)

// BulkSharePercent is the most of a process's workers the bulk lane may hold
// at once, and the most of each send budget limit it may take; the rest is
// kept for the system lane. With three workers bulk sends hold two and a
// single mail always finds the third free; of 25 mails a minute bulk sends
// take 17.
const BulkSharePercent = 70

// bulkShare is the bulk lane's share of n workers or of a limit of n:
// BulkSharePercent of it, rounded down, and at least one, or a lone worker
// or a limit of one would never send a list.
func bulkShare(n int) int {
	if share := n * BulkSharePercent / 100; share > 0 {
		return share
	}
	return 1
}

// bulkSlotsFor is how many of workers the bulk lane may hold.
func bulkSlotsFor(workers int) int { return bulkShare(workers) }

type RecipientInfo struct {
	FullName string
	Email    string
	// Sub is the person's Keycloak subject when the send knows it (a
	// Keycloak group's member), written to mail_queue.recipient_sub; nil
	// when only an address is known. A mail never renders it.
	Sub *uuid.UUID
	// UnsubscribeURL is the person's one-click unsubscribe link when core
	// gave one (a notification's unsubscribe_url): the row keeps it for
	// List-Unsubscribe (list_unsubscribe.go) and the mail renders it as
	// .UnsubscribeUrl. Empty for none.
	UnsubscribeURL string
}

type EnqueueWithRecipientsParams struct {
	FormSourceID  *uuid.UUID
	SentBy        string
	TemplateID    uuid.UUID
	MailListID    *uuid.UUID
	BodyVariables []byte
	Recipients    []RecipientInfo
	// Tracking asks for open and click tracking (tracking.go); it is ignored
	// while SKYMAIL_TRACKING is off.
	Tracking bool
	// System says the send is a service's mail (core's notification): with
	// SKYMAIL_TRACKING_SYSTEM on it is tracked whatever Tracking says, its
	// Preferences what ResolveSystemTracking said; off, it is not tracked.
	System bool
	// Preferences is whom core lets be tracked, asked before the send's
	// transaction (ResolveTracking, ResolveSystemTracking).
	// Mailer.EnqueueWithRecipients asks itself when it is nil; a send queued
	// in a caller's transaction (Queue) never asks, and tracks no one core was
	// not asked about.
	Preferences *TrackingPreferences
}

type Mailer interface {
	// Start puts back to pending the processing rows whose lease has run out,
	// keeps doing so while ctx lives, and, unless the sender is paused, starts
	// the dispatcher and workerCount workers.
	Start(ctx context.Context, workerCount int)
	// SenderPaused says MAIL_SENDER=paused holds this process's sender back:
	// mail is queued but none is sent.
	SenderPaused() bool
	// TrackingAvailable says SKYMAIL_TRACKING=on: a send may ask for open
	// and click tracking. Off, the request is ignored.
	TrackingAvailable() bool
	// TrackingApplies says a send of templateID to mailListID (nil: to one
	// person) that asks for tracking gets it: the flag is on and the
	// template is neither a System template nor sensitive. Its answer is
	// what a send's tracking says.
	TrackingApplies(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID) bool
	// SystemTrackingAvailable says SKYMAIL_TRACKING_SYSTEM is on with
	// SKYMAIL_TRACKING: the mail services send (EnqueueSystemSingle,
	// EnqueueWithRecipientsParams.System) is tracked without asking.
	SystemTrackingAvailable() bool
	Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error)
	EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error)
	// EnqueueSystemSingle queues a mail a service sends one person (a login
	// code, a Forms confirmation, an approval notice) in the system lane, as
	// EnqueueSingle does, and with SKYMAIL_TRACKING_SYSTEM on tracks it:
	// before the transaction the person's account is found in Keycloak by
	// address and core is asked (ResolveSystemTracking); the mail gets the
	// pixel, and a click address only on the links its template writes out
	// as they are, none in a sensitive mail. It says whether the send is
	// tracked; off, it is EnqueueSingle.
	EnqueueSystemSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, bool, error)
	// EnqueueTrackedSingle queues a mail a person sends one person and asks
	// tracking for (POST /mail_tasks/single with tracking): an operator's
	// mail, so in the bulk lane, where tracking is planned. The system lane
	// is never tracked as asked (only a service's mail is, by
	// EnqueueSystemSingle). preferences is what ResolveTracking said about the
	// recipient; nil, and it asks itself before the transaction begins
	// (finds their account by address, asks core).
	EnqueueTrackedSingle(ctx context.Context, arg database.CreateSingleMailTaskParams, preferences *TrackingPreferences) (uuid.UUID, error)
	EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error)
}

// Mailer's ways to queue a send pick the lane: Enqueue (a list),
// EnqueueWithRecipients (a group) and EnqueueTrackedSingle the bulk lane,
// EnqueueSingle the system lane.

// Queue is the send path inside a caller's transaction: the same ways
// to queue a send as Mailer's, written through that transaction, so the send
// and its queue rows exist only if it commits, and nothing is dispatched
// before it has. All queue in the lane the caller chose. The caller
// wakes the dispatcher once it has committed.
type Queue interface {
	Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error)
	EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error)
	// EnqueueTrackedSingle queues a mail to one person that asked for
	// tracking, with what ResolveTracking said about them before the
	// transaction (their account by address, core's answer). Keycloak and
	// core are never asked here: nil tracks no one.
	EnqueueTrackedSingle(ctx context.Context, arg database.CreateSingleMailTaskParams, preferences *TrackingPreferences) (uuid.UUID, error)
	EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error)
}

// Transactional is a Mailer that can also queue a send in a caller's
// transaction. Mailer's own methods write through the pool, statement by
// statement, and wake the dispatcher, as they always have.
type Transactional interface {
	Mailer
	// Queue writes sends through q, a transaction's queries, in the lane
	// priority.
	Queue(q *database.Queries, priority Priority) Queue
	// ResolveTracking asks core whom of recipients a send of templateID to
	// mailListID may track. Call it before the transaction a Queue writes
	// in and pass the answer as EnqueueWithRecipientsParams.Preferences:
	// SkyMail never waits for core holding a transaction.
	ResolveTracking(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID, recipients []RecipientInfo) *TrackingPreferences
	// ResolvePeopleTracking is ResolveTracking for people a send names one
	// by one: their accounts are first found in Keycloak by address. Call it
	// before the transaction too, and pass the answer to
	// Queue.EnqueueTrackedSingle.
	ResolvePeopleTracking(ctx context.Context, templateID uuid.UUID, recipients []RecipientInfo) *TrackingPreferences
	// ResolveSystemTracking asks Keycloak (for those known only by address)
	// and core whom of the recipients of a service's mail it may track, all
	// within 2 seconds; nil when SKYMAIL_TRACKING_SYSTEM is off. Call it
	// before the transaction and pass the answer with
	// EnqueueWithRecipientsParams.System.
	ResolveSystemTracking(ctx context.Context, recipients []RecipientInfo) *TrackingPreferences
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
	// Stopped closes once the dispatcher, the workers and the reaper Start
	// began have all returned: after Stop, or once Start's context has ended
	// and the sends it cut off have given up. It never closes without Start.
	Stopped() <-chan struct{}
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
	// bulkSlots is how many rows of the bulk lane this process holds at most
	// (bulkSlotsFor), and takenBulk how many of taken are bulk rows.
	bulkSlots int
	takenBulk atomic.Int32
	// sendBudget bounds one SMTP send from dial to QUIT (SendBudget).
	sendBudget time.Duration
	// budget is the send budget all processes share (RateBudget); clock,
	// when a test sets it, is the instant the budget reads instead of the
	// database's clock.
	budget RateBudget
	clock  func() time.Time
	// channels makes a worker's sender of each channel it sends (channel.go);
	// tests replace them.
	channels map[string]newChannelSender
	// clientOptions are added to the workers' SMTP client options; tests
	// set them.
	clientOptions []mail.Option
	// tracking is open and click tracking (WithTracking); off by default.
	tracking Tracking
	// refuseReserved writes a recipient in a reserved domain failed when a
	// send is queued (WithReservedDomainsRefused); off by default.
	refuseReserved bool
	// lanes are the sender lanes (WithSendLanes; lanes.go): every lane off
	// by default. templateKey reads a send's template for a mail's lane;
	// graphTokens and exchangeTokens are the application's tokens, shared
	// by the workers; graphURL and graphClient are where Graph is.
	lanes          SendLanes
	templateKey    templateKeyOf
	graphTokens    tokenSource
	exchangeTokens tokenSource
	graphURL       string
	graphClient    *http.Client

	// stop closes when Stop is called; stopOnce closes it. dispatching,
	// sending and reaping are the dispatcher, the workers and the reaper,
	// which Stop waits for.
	stop        chan struct{}
	stopOnce    sync.Once
	dispatching sync.WaitGroup
	sending     sync.WaitGroup
	reaping     sync.WaitGroup
	// stopped closes once the dispatcher, the workers and the reaper have
	// all returned.
	stopped chan struct{}
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
		stopped:    make(chan struct{}),
		graphURL:   DefaultGraphURL,
		// Each request is bounded by the send's own context (sendBudget).
		graphClient: &http.Client{},
	}
	m.templateKey = m.databaseTemplateKey
	m.channels = map[string]newChannelSender{ChannelEmail: m.newEmailSender}
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
		m.logger.Info().Int("workers", workerCount).Int("bulk_slots", bulkSlotsFor(workerCount)).
			Str("sender", string(m.sender)).Str("claimant", m.claimant).Dur("lease", m.lease).
			Int("rate_per_minute", m.budget.PerMinute).Int("daily_recipient_budget", m.budget.PerDay).
			Msg("Starting up")
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
		m.closeWhenStopped()
		return
	}

	m.workers = workerCount
	m.bulkSlots = bulkSlotsFor(workerCount)
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
	m.closeWhenStopped()
}

// closeWhenStopped closes stopped once everything start began has returned.
func (m *mailerImpl) closeWhenStopped() {
	go func() {
		m.dispatching.Wait()
		m.sending.Wait()
		m.reaping.Wait()
		close(m.stopped)
	}()
}

func (m *mailerImpl) Stopped() <-chan struct{} { return m.stopped }

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
	defer m.untake(job)
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
			m.pruneSendWindows(ctx)
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
	RecipientSub      *uuid.UUID
	UnsubscribeURL    string
}

// queue writes sends through q: the pool's queries, or a transaction's, in
// the lane priority.
type queue struct {
	q        *database.Queries
	logger   *zerolog.Logger
	priority Priority
	tracking *Tracking
	// refuseReserved is the mailer's (WithReservedDomainsRefused).
	refuseReserved bool
}

func (m *mailerImpl) Queue(q *database.Queries, priority Priority) Queue {
	return &queue{q: q, logger: m.logger, priority: priority, tracking: &m.tracking, refuseReserved: m.refuseReserved}
}

func (m *mailerImpl) Wake() { m.wake() }

// pooled writes a send in the lane priority in a transaction of its own, so
// the send and its queue rows exist together or not at all: a send that fails
// to queue leaves no send without rows behind. It wakes the dispatcher once
// rows are queued.
func (m *mailerImpl) pooled(ctx context.Context, priority Priority, write func(*queue) (uuid.UUID, bool, error)) (uuid.UUID, error) {
	var id uuid.UUID
	var queued bool
	err := m.db.InTx(ctx, func(q *database.Queries) error {
		var err error
		id, queued, err = write(&queue{q: q, logger: m.logger, priority: priority, tracking: &m.tracking, refuseReserved: m.refuseReserved})
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	m.wakeIf(queued)
	return id, nil
}

func (m *mailerImpl) Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error) {
	return m.pooled(ctx, PriorityBulk, func(q *queue) (uuid.UUID, bool, error) {
		return q.enqueue(ctx, arg)
	})
}

func (m *mailerImpl) EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	return m.pooled(ctx, PrioritySystem, func(q *queue) (uuid.UUID, bool, error) {
		return q.enqueueSingle(ctx, arg, nil, false)
	})
}

func (m *mailerImpl) EnqueueSystemSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, bool, error) {
	tracked := m.SystemTrackingAvailable() && arg.TemplateID != nil
	if !tracked {
		id, err := m.EnqueueSingle(ctx, arg)
		return id, false, err
	}
	// Keycloak and core are asked before the transaction begins.
	preferences := m.ResolveSystemTracking(ctx, []RecipientInfo{{FullName: arg.RecipientFullName, Email: arg.RecipientEmail}})
	id, err := m.pooled(ctx, PrioritySystem, func(q *queue) (uuid.UUID, bool, error) {
		return q.enqueueSingle(ctx, arg, preferences, true)
	})
	return id, err == nil, err
}

func (m *mailerImpl) EnqueueTrackedSingle(ctx context.Context, arg database.CreateSingleMailTaskParams, preferences *TrackingPreferences) (uuid.UUID, error) {
	// Keycloak and core are asked before the transaction begins.
	if preferences == nil && arg.TrackingRequested && arg.TemplateID != nil {
		preferences = m.ResolvePeopleTracking(ctx, *arg.TemplateID, []RecipientInfo{{FullName: arg.RecipientFullName, Email: arg.RecipientEmail}})
	}
	return m.pooled(ctx, PriorityBulk, func(q *queue) (uuid.UUID, bool, error) {
		return q.enqueueSingle(ctx, arg, preferences, false)
	})
}

func (m *mailerImpl) EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error) {
	// Core is asked before the transaction begins.
	switch {
	case params.Preferences != nil:
	case params.System:
		params.Preferences = m.ResolveSystemTracking(ctx, params.Recipients)
	case params.Tracking && m.TrackingAvailable():
		params.Preferences = m.ResolveTracking(ctx, params.TemplateID, params.MailListID, params.Recipients)
	}
	return m.pooled(ctx, PriorityBulk, func(q *queue) (uuid.UUID, bool, error) {
		return q.enqueueWithRecipients(ctx, params)
	})
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
	id, _, err := q.enqueueSingle(ctx, arg, nil, false)
	return id, err
}

func (q *queue) EnqueueTrackedSingle(ctx context.Context, arg database.CreateSingleMailTaskParams, preferences *TrackingPreferences) (uuid.UUID, error) {
	id, _, err := q.enqueueSingle(ctx, arg, preferences, false)
	return id, err
}

func (q *queue) EnqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, error) {
	id, _, err := q.enqueueWithRecipients(ctx, params)
	return id, err
}

// The three ways to queue a send return the send and whether queue rows were
// written, which is when the dispatcher has something to do.

func (q *queue) enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, bool, error) {
	kind := q.tracking.trackingOf(arg.TrackingRequested, false)
	arg.TrackingRequested = kind != trackNone
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

	queued, err := q.renderAndQueue(ctx, commonRows, false, kind, nil)
	return rows[0].TaskID, queued, err
}

// enqueueSingle queues a mail to one person; system says it is a service's
// mail, tracked as the system (SKYMAIL_TRACKING_SYSTEM) and never as asked.
// preferences, when it is tracked, is what ResolveTracking or
// ResolveSystemTracking found: the person's account, if Keycloak found one
// by address, becomes the row's recipient_sub.
func (q *queue) enqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams, preferences *TrackingPreferences, system bool) (uuid.UUID, bool, error) {
	kind := q.tracking.trackingOf(arg.TrackingRequested, system)
	arg.TrackingRequested = kind != trackNone
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
	if arg.TrackingRequested {
		commonRows[0].RecipientSub, _ = preferences.accountOf(row.RecipientEmail)
	}

	// A single mail has one recipient, and its caller (a login code, a Forms
	// confirmation) is waiting for it: a mail that does not render is the
	// send failing, and the caller is told.
	queued, err := q.renderAndQueue(ctx, commonRows, true, kind, preferences)
	return row.TaskID, queued, err
}

func (q *queue) enqueueWithRecipients(ctx context.Context, params EnqueueWithRecipientsParams) (uuid.UUID, bool, error) {
	kind := q.tracking.trackingOf(params.Tracking, params.System)
	task, err := q.q.InsertMailTask(ctx, database.InsertMailTaskParams{
		SentBy:            params.SentBy,
		TemplateID:        &params.TemplateID,
		MailListID:        params.MailListID,
		FormSourceID:      params.FormSourceID,
		BodyVariables:     params.BodyVariables,
		TrackingRequested: kind != trackNone,
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
			RecipientSub:      r.Sub,
			UnsubscribeURL:    r.UnsubscribeURL,
		})
	}

	queued, err := q.renderAndQueue(ctx, rows, false, kind, params.Preferences)
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
// FullName over them, and UnsubscribeUrl when the recipient has a link. A
// part that does not parse is a *ParseError.
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
	return t.renderWith(vars, vars, recipient)
}

// renderWith renders the subject and the plain text with textVars and the
// HTML with htmlVars: a masked mail (mask.go) marks what it hid in the HTML
// only. A send passes the same variables to both.
func (t mailTemplates) renderWith(textVars, htmlVars map[string]interface{}, recipient RecipientInfo) (Rendered, error) {
	withRecipient := func(vars map[string]interface{}) map[string]interface{} {
		data := make(map[string]interface{}, len(vars)+2)
		for k, v := range vars {
			data[k] = v
		}
		data["Email"] = recipient.Email
		data["FullName"] = recipient.FullName
		if recipient.UnsubscribeURL != "" {
			data["UnsubscribeUrl"] = recipient.UnsubscribeURL
		}
		return data
	}
	textData, htmlData := withRecipient(textVars), withRecipient(htmlVars)

	var subject, text, html bytes.Buffer
	if err := t.subject.Execute(&subject, textData); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartSubject, err)
	}
	if err := t.text.Execute(&text, textData); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartPlainText, err)
	}
	if err := t.html.Execute(&html, htmlData); err != nil {
		return Rendered{}, fmt.Errorf("render %s: %w", PartHTML, err)
	}
	return Rendered{Subject: subject.String(), PlainText: text.String(), HTML: html.String()}, nil
}

// renderAndQueue renders each row's mail and writes the queue rows, saying
// whether any is pending. A recipient whose mail does not render (a template
// that slices an empty name) is written failed with the render error, and the
// others are queued: one recipient never holds back a send to the rest. With
// strict, a mail that does not render is returned as the error and nothing is
// written. With refuseReserved, a recipient in a reserved domain (a seed
// account at example.com) is written failed with reservedDomainError, strict
// or not, and never reaches the relay. A recipient at a suppressed address
// (suppression.go) is written failed with suppressedError the same way,
// unless the send is account mail (AccountMail): then it is queued, its row
// flagged suppressed_but_sent. kind says how the send is tracked (asked for
// with the flag on, or a service's mail with SKYMAIL_TRACKING_SYSTEM on):
// each queued row gets a token or why not, as preferences — what core said
// before the transaction — allows, and the links a click is counted on are
// registered (tracking.go).
func (q *queue) renderAndQueue(ctx context.Context, rows []commonMailRow, strict bool, kind trackingKind, preferences *TrackingPreferences) (bool, error) {
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

	suppressed, accountMail, err := q.suppressionsOf(ctx, rows)
	if err != nil {
		return false, err
	}
	var flagged []string

	queueItems := make([]database.CreateMailQueueItemsParams, 0, len(rows))
	var failed []database.CreateFailedMailQueueItemParams
	for _, row := range rows {
		if q.refuseReserved && reservedDomain(row.RecipientEmail) {
			q.logger.Warn().Str("task_id", row.TaskID.String()).
				Msg("A recipient is in a reserved domain: written failed, never sent; the others are queued")
			failed = append(failed, database.CreateFailedMailQueueItemParams{
				TaskID:            row.TaskID,
				RecipientFullName: row.RecipientFullName,
				RecipientEmail:    row.RecipientEmail,
				Priority:          int16(q.priority),
				Error:             reservedDomainError,
				RecipientSub:      row.RecipientSub,
			})
			continue
		}
		suppression, isSuppressed := suppressed[suppressionAddress(row.RecipientEmail)]
		if isSuppressed && !accountMail {
			q.logger.Warn().Str("task_id", row.TaskID.String()).Str("suppression", suppression.Source+" "+suppression.ReasonCode).
				Msg("A recipient's address is suppressed: written failed, never sent; the others are queued")
			failed = append(failed, database.CreateFailedMailQueueItemParams{
				TaskID:            row.TaskID,
				RecipientFullName: row.RecipientFullName,
				RecipientEmail:    row.RecipientEmail,
				Priority:          int16(q.priority),
				Error:             suppressedError(suppression),
				RecipientSub:      row.RecipientSub,
			})
			continue
		}
		rendered, err := parsed.render(taskVars, RecipientInfo{FullName: row.RecipientFullName, Email: row.RecipientEmail, UnsubscribeURL: row.UnsubscribeURL})
		if err != nil {
			if strict {
				return false, err
			}
			q.logger.Err(err).Str("task_id", row.TaskID.String()).
				Msg("A recipient's mail did not render: written failed, the others are queued")
			failed = append(failed, database.CreateFailedMailQueueItemParams{
				TaskID:            row.TaskID,
				RecipientFullName: row.RecipientFullName,
				RecipientEmail:    row.RecipientEmail,
				Priority:          int16(q.priority),
				Error:             err.Error(),
				RecipientSub:      row.RecipientSub,
			})
			continue
		}

		htmlRes := rendered.HTML

		queueItems = append(queueItems, database.CreateMailQueueItemsParams{
			TaskID:             row.TaskID,
			RecipientFullName:  row.RecipientFullName,
			RecipientEmail:     row.RecipientEmail,
			Subject:            rendered.Subject,
			Body:               rendered.PlainText,
			BodyHtml:           &htmlRes,
			Priority:           int16(q.priority),
			RecipientSub:       row.RecipientSub,
			SuppressedButSent:  isSuppressed,
			ListUnsubscribeUrl: nonEmpty(row.UnsubscribeURL),
		})
		if isSuppressed {
			q.logger.Warn().Str("task_id", row.TaskID.String()).Str("suppression", suppression.Source+" "+suppression.ReasonCode).
				Msg("Account mail to a suppressed address: queued and flagged")
			flagged = append(flagged, suppression.Address)
		}
	}

	if kind != trackNone && len(queueItems) > 0 {
		rule, err := q.planTracking(ctx, rows[0].TaskID, queueItems, preferences, kind)
		if err != nil {
			return false, err
		}
		if err := q.registerLinks(ctx, rows[0].TaskID, queueItems, rule, rows[0].HtmlContent); err != nil {
			return false, err
		}
	}
	if len(queueItems) > 0 {
		if _, err = q.q.CreateMailQueueItems(ctx, queueItems); err != nil {
			return false, err
		}
	}
	for _, item := range failed {
		if err := q.q.CreateFailedMailQueueItem(ctx, item); err != nil {
			return false, err
		}
	}
	if len(flagged) > 0 {
		if err := q.q.FlagSuppressionsExempt(ctx, flagged); err != nil {
			return false, err
		}
	}
	return len(queueItems) > 0, nil
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

// drainQueue takes as many due rows as this process has idle workers: the
// system lane's first, then bulk rows while the bulk lane holds fewer than
// bulkSlots and the send budget leaves room. A worker that finishes a row
// wakes the dispatcher, which takes the next. A bulk lane at its budget waits
// for the tick: room opens as the windows slide.
func (m *mailerImpl) drainQueue(ctx context.Context, logger *zerolog.Logger) bool {
	// The tick or a nudge may win the select over a stop already made.
	if m.stopping() {
		return false
	}
	idle := m.workers - int(m.taken.Load())
	if idle <= 0 {
		return true
	}

	items := m.claim(ctx, logger, true, idle)
	system := len(items)
	bulkRoom := min(idle-len(items), m.bulkSlots-int(m.takenBulk.Load()))
	if bulkRoom > 0 {
		bulk := m.claimBulk(ctx, logger, bulkRoom)
		m.takenBulk.Add(int32(len(bulk)))
		items = append(items, bulk...)
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
	// Counted once they are with the workers: the budget never holds a
	// system row back.
	m.countSystem(ctx, logger, system)

	return true
}

// claim takes up to maxRows due rows of one lane for this process, outside
// the send budget: the system lane always, the bulk lane when there is no
// budget (claimBulk).
func (m *mailerImpl) claim(ctx context.Context, logger *zerolog.Logger, systemLane bool, maxRows int) []database.MailQueue {
	var items []database.MailQueue
	var err error
	if systemLane {
		items, err = m.db.ProcessSystemQueueItems(ctx, database.ProcessSystemQueueItemsParams{
			ClaimedBy: m.claimant,
			MaxRows:   maxRows,
		})
	} else {
		items, err = m.db.ProcessBulkQueueItems(ctx, database.ProcessBulkQueueItemsParams{
			ClaimedBy: m.claimant,
			MaxRows:   maxRows,
		})
	}
	if err != nil {
		logger.Err(err).Bool("system_lane", systemLane).Msg("Failed to process queue items")
	}
	return items
}

// untake counts job, which this process took, as no longer held: sent,
// failed, put back for a retry or given back.
func (m *mailerImpl) untake(job database.MailQueue) {
	if Priority(job.Priority) <= PriorityBulk {
		m.takenBulk.Add(-1)
	}
	m.taken.Add(-1)
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

	senders, err := m.newSenders()
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
			m.send(ctx, senders, &logger, job)

			// This worker is idle again: the dispatcher may take a row for it.
			m.untake(job)
			m.wake()
		}
	}
}

// outcomeTimeout bounds writing a send's outcome.
const outcomeTimeout = 5 * time.Second

// send sends job through its channel's sender and records the outcome. The
// outcome is written even when ctx ended as the send finished: a mail the
// relay took and the row not saying so would go out again after the lease. A
// send cut off because ctx ended (the process going away) is no failed
// attempt: the row keeps its claim and is put back when the lease runs out. A
// row of a channel this process has no sender for fails for good.
func (m *mailerImpl) send(ctx context.Context, senders map[string]ChannelSender, logger *zerolog.Logger, job database.MailQueue) {
	sender, known := senders[job.Channel]
	var err error
	if known {
		err = sender.Send(ctx, job)
	} else {
		err = unknownChannel(job.Channel)
	}
	if err != nil && known && ctx.Err() != nil {
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
		} else {
			m.suppressRefused(ctx, logger, job, sendErr)
		}
		return
	}

	delay := retryDelay(job.Attempts)
	// A relay that said when to come back (Graph's Retry-After on a 429 or
	// a 503) is not tried sooner.
	var later interface{ RetryAfter() time.Duration }
	if errors.As(sendErr, &later) && later.RetryAfter() > delay {
		delay = later.RetryAfter()
	}
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

// nonEmpty is s, or nil when it is empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
