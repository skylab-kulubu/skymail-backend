package mailer

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"time"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// SetDeliver makes m send a row with deliver instead of SMTP. Call it before
// Start.
func SetDeliver(m Transactional, deliver func(ctx context.Context, job database.MailQueue) error) {
	SetChannelSender(m, ChannelEmail, deliver)
}

// Claimant is the name m writes in the rows it takes (claimed_by).
func Claimant(m Transactional) string { return m.(*mailerImpl).claimant }

// SetSendBudget bounds m's SMTP sends to budget instead of SendBudget. Call it
// before Start.
func SetSendBudget(m Transactional, budget time.Duration) { m.(*mailerImpl).sendBudget = budget }

// BulkSlotsFor is how many of a process's workers the bulk lane may hold.
var BulkSlotsFor = bulkSlotsFor

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

// SendOnce sends job the way a worker does (its e-mail sender),
// without a queue or a database.
func SendOnce(ctx context.Context, m Transactional, job database.MailQueue) error {
	sender, err := m.(*mailerImpl).newEmailSender()
	if err != nil {
		return err
	}
	return sender.Send(ctx, job)
}

// WindowRoom is the room one send window leaves the bulk lane.
var WindowRoom = windowRoom

// SetClock makes m's send budget read now instead of the database's clock.
// Call it before Start.
func SetClock(m Transactional, now func() time.Time) { m.(*mailerImpl).clock = now }

// PrepareTurns readies m to take rows a turn at a time (Turn) for slots
// workers, none of them running.
func PrepareTurns(m Transactional, slots int) {
	impl := m.(*mailerImpl)
	impl.workers = slots
	impl.bulkSlots = bulkSlotsFor(slots)
	impl.jobs = make(chan database.MailQueue, slots)
}

// Turn runs one turn of m's dispatcher and returns the rows it took, which
// then count as sent: the next turn has every worker idle again.
func Turn(ctx context.Context, m Transactional) []database.MailQueue {
	impl := m.(*mailerImpl)
	impl.drainQueue(ctx, impl.logger)
	var taken []database.MailQueue
	for {
		select {
		case job := <-impl.jobs:
			taken = append(taken, job)
			impl.untake(job)
		default:
			return taken
		}
	}
}

// BulkRoom is the room budget leaves the bulk lane at at, given the windows
// counted.
func BulkRoom(budget RateBudget, at time.Time, counted []database.MailSendWindow) int {
	return budget.bulkRoom(at, counted)
}

// PruneSendWindows drops the send windows m's budget reads no more, as the
// reaper does.
func PruneSendWindows(ctx context.Context, m Transactional) { m.(*mailerImpl).pruneSendWindows(ctx) }

// SetMaskSendHook runs hook before each send the masking sweep masks, until
// the returned function is called.
func SetMaskSendHook(hook func(uuid.UUID)) func() {
	maskSendHook = hook
	return func() { maskSendHook = nil }
}

// SetChannelSender makes m's workers send the rows of channel with send. Call
// it before Start.
func SetChannelSender(m Transactional, channel string, send func(ctx context.Context, job database.MailQueue) error) {
	m.(*mailerImpl).channels[channel] = func() (ChannelSender, error) { return channelSenderFunc(send), nil }
}

// IsPermanent says err fails its row for good at once: no retry.
func IsPermanent(err error) bool {
	var permanent permanentError
	return errors.As(err, &permanent)
}

// ReservedDomain says address is in a domain reserved for tests and examples.
var ReservedDomain = reservedDomain

// TokenSource is a scope's access token, as the sender lanes ask for it.
type TokenSource = tokenSource

// SetGraph makes m's Graph lanes send to url with client and take their
// tokens from tokens. Call it after WithSendLanes, before Start.
func SetGraph(m Transactional, url string, client *http.Client, tokens TokenSource) {
	impl := m.(*mailerImpl)
	impl.graphURL, impl.graphClient, impl.graphTokens = url, client, tokens
}

// SetExchangeTokens makes m's XOAUTH2 lanes take their tokens from tokens.
func SetExchangeTokens(m Transactional, tokens TokenSource) { m.(*mailerImpl).exchangeTokens = tokens }

// SetTemplateKeys makes m read a send's template key, which picks a mail's
// sender lane, with keyOf instead of the database.
func SetTemplateKeys(m Transactional, keyOf func(ctx context.Context, taskID uuid.UUID) (*string, bool, error)) {
	m.(*mailerImpl).templateKey = func(ctx context.Context, taskID uuid.UUID) (sendLane, error) {
		key, system, err := keyOf(ctx, taskID)
		return sendLane{Key: key, System: system}, err
	}
}

// SetSendLanes is SetTemplateKeys with the send's purpose too.
func SetSendLanes(m Transactional, laneOf func(ctx context.Context, taskID uuid.UUID) (key *string, system bool, purpose *string, err error)) {
	m.(*mailerImpl).templateKey = func(ctx context.Context, taskID uuid.UUID) (sendLane, error) {
		key, system, purpose, err := laneOf(ctx, taskID)
		return sendLane{Key: key, System: system, Purpose: purpose}, err
	}
}

// GraphFailure classifies Graph's answer to a send.
var GraphFailure = graphFailure

// RetryAfterOf reads a Retry-After header.
var RetryAfterOf = retryAfter

// SetConsentTiming sets the consent cache's lifetime, a fetch's limit, how
// long core's breaker stays open, how soon a row waiting for a fetch is
// taken again, and its clock (nil: time.Now).
func SetConsentTiming(m Transactional, ttl, round, breaker, fetchDelay time.Duration, now func() time.Time) {
	c := m.(*mailerImpl).consentCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl, c.roundTimeout, c.breakerOpen, c.fetchDelay = ttl, round, breaker, fetchDelay
	if now != nil {
		c.now = now
	}
}

// WaitConsentFetches waits for the consent fetches under way.
func WaitConsentFetches(m Transactional) { m.(*mailerImpl).consentFetches.Wait() }

// SetPreferenceTiming sets the bulk preference check's cache, fetch and
// breaker times, and its clock when now is not nil.
func SetPreferenceTiming(m Transactional, ttl, round, breaker, fetchDelay time.Duration, now func() time.Time) {
	c := m.(*mailerImpl).preferenceCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl, c.roundTimeout, c.breakerOpen, c.fetchDelay = ttl, round, breaker, fetchDelay
	if now != nil {
		c.now = now
	}
}

// WaitPreferenceFetches waits for the bulk preference fetches under way.
func WaitPreferenceFetches(m Transactional) { m.(*mailerImpl).preferenceFetches.Wait() }

// SMTPReply is the error a relay's reply to stage makes, as a worker's send
// returns it.
func SMTPReply(stage string, code int, enhanced string) error {
	reply := smtpReply{stage: stage, code: code, enhanced: enhanced}
	if reply.permanent() {
		return permanentError{reply}
	}
	return reply
}

// GraphReply is the error Graph's answer status makes, with its Retry-After.
func GraphReply(status int, retryAfter time.Duration) error {
	return graphReply{status: status, retryAfter: retryAfter}
}

// Throttled says err is the relay asking the account to slow down.
var Throttled = throttled

// SendRow sends job, which m took, the way a worker does, and records the
// outcome.
func SendRow(ctx context.Context, m Transactional, job database.MailQueue) error {
	impl := m.(*mailerImpl)
	senders, err := impl.newSenders()
	if err != nil {
		return err
	}
	impl.send(ctx, senders, impl.logger, job)
	return nil
}

// SweepCancelled fails the pending rows of cancelled sends, as the reaper
// does.
func SweepCancelled(ctx context.Context, m Transactional) { m.(*mailerImpl).sweepCancelled(ctx) }

// UnparkResumed puts back due the parked rows of sends no longer paused, as
// the reaper does.
func UnparkResumed(ctx context.Context, m Transactional) { m.(*mailerImpl).unparkResumed(ctx) }

// SkipHeldTasks hands back, of rows m took in q's transaction, those of a
// paused or a cancelled send, as a take does.
func SkipHeldTasks(ctx context.Context, m Transactional, q *database.Queries, rows []database.MailQueue) ([]database.MailQueue, error) {
	impl := m.(*mailerImpl)
	return impl.skipHeldTasks(ctx, q, impl.logger, rows)
}

// MaxRowThrottles is how many throttling replies a bulk row may have with no
// attempt counted.
const MaxRowThrottles = maxRowThrottles
