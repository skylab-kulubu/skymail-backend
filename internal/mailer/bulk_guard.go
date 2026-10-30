package mailer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The bulk lane's guards (hub ticket duyuru-haber 05, G3; README, "Kısma ve
// görev düzeyinde işlemler"):
//
//   - a throttling reply of the relay (SMTP 421, 432 or 4.7.x; Graph 429 or
//     503) holds every process's bulk lane for SKYMAIL_THROTTLE_PAUSE, or the
//     reply's Retry-After if longer, and puts a bulk row it answered back to
//     pending with no attempt counted: the reply is about the account's pace,
//     not the recipient. A system row keeps its retries as before, so a login
//     code never waits on the bulk lane's hold;
//   - a send whose recipients fail for good at the relay past
//     SKYMAIL_FAILURE_PAUSE_COUNT, and past SKYMAIL_FAILURE_PAUSE_PERCENT of
//     those the relay answered, is paused (the listmonk pattern): its rows
//     wait until an operator resumes or cancels it.
const (
	ThrottlePauseEnv       = "SKYMAIL_THROTTLE_PAUSE"
	FailurePauseCountEnv   = "SKYMAIL_FAILURE_PAUSE_COUNT"
	FailurePausePercentEnv = "SKYMAIL_FAILURE_PAUSE_PERCENT"

	DefaultThrottlePause       = 5 * time.Minute
	DefaultFailurePauseCount   = 10
	DefaultFailurePausePercent = 10

	// maxThrottlePause bounds SKYMAIL_THROTTLE_PAUSE: past an hour a throttled
	// account is a matter for an operator, not for waiting.
	maxThrottlePause = time.Hour

	// maxRowThrottles is how many throttling replies a bulk row may have with
	// no attempt counted (mail_queue.throttles). Its next one is retried as
	// any other failure and holds nothing: a relay answering one recipient
	// 4.7.x for good fails that row in the end, and never holds the bulk lane
	// every few minutes for ever.
	maxRowThrottles = 5
)

// BulkGuard is the bulk lane's guards. The zero value guards nothing: a
// throttling reply is retried as any other, and no send is paused.
type BulkGuard struct {
	// ThrottlePause is how long a throttling reply holds the bulk lane at
	// least; 0 turns the hold off.
	ThrottlePause time.Duration
	// FailureCount and FailurePercent pause a send whose recipients failed
	// for good at the relay at least FailureCount times and at least
	// FailurePercent percent of those the relay answered; a FailureCount of
	// 0 never pauses one.
	FailureCount   int
	FailurePercent int
}

// BulkGuardFromEnv reads SKYMAIL_THROTTLE_PAUSE (a Go duration, unset: 5m, 0
// for no hold, at most 1h), SKYMAIL_FAILURE_PAUSE_COUNT (a whole number,
// unset: 10, 0 never pauses) and SKYMAIL_FAILURE_PAUSE_PERCENT (1 to 100,
// unset: 10). Anything else is an error that stops startup.
func BulkGuardFromEnv(getenv func(string) string) (BulkGuard, error) {
	guard := BulkGuard{
		ThrottlePause:  DefaultThrottlePause,
		FailureCount:   DefaultFailurePauseCount,
		FailurePercent: DefaultFailurePausePercent,
	}
	if raw := strings.TrimSpace(getenv(ThrottlePauseEnv)); raw != "" {
		pause, err := time.ParseDuration(raw)
		if raw == "0" {
			pause, err = 0, nil
		}
		if err != nil || pause < 0 || pause > maxThrottlePause {
			return BulkGuard{}, fmt.Errorf("%s must be a duration from 0 to 1h (5m, 90s), not %q", ThrottlePauseEnv, raw)
		}
		guard.ThrottlePause = pause
	}
	count, err := limitFromEnv(getenv, FailurePauseCountEnv, DefaultFailurePauseCount)
	if err != nil {
		return BulkGuard{}, err
	}
	guard.FailureCount = count
	if raw := strings.TrimSpace(getenv(FailurePausePercentEnv)); raw != "" {
		percent, err := strconv.Atoi(raw)
		if err != nil || percent < 1 || percent > 100 {
			return BulkGuard{}, fmt.Errorf("%s must be a whole number from 1 to 100, not %q", FailurePausePercentEnv, raw)
		}
		guard.FailurePercent = percent
	}
	return guard, nil
}

// WithBulkGuard sets the bulk lane's guards. Without it the mailer has none.
func WithBulkGuard(guard BulkGuard) Option {
	return func(m *mailerImpl) { m.guard = guard }
}

// throttleText finds a throttling reply in the text of a send that failed
// before go-mail could name its stage (a 421 at the greeting, to AUTH).
var throttleText = regexp.MustCompile(`\b(421|432)\b|\b4\.7\.\d{1,3}\b`)

// throttled says err is the relay asking the account to slow down: SMTP 421
// (service not available, Exchange Online's "too many connections" and
// "concurrent submissions"), 432, or any 4.7.x; Graph 429 or 503.
func throttled(err error) bool {
	var smtp smtpReply
	if errors.As(err, &smtp) {
		return smtp.code == 421 || smtp.code == 432 || strings.HasPrefix(smtp.enhanced, "4.7.")
	}
	var graph graphReply
	if errors.As(err, &graph) {
		return graph.status == 429 || graph.status == 503
	}
	return strings.HasPrefix(err.Error(), "send failed: ") && throttleText.MatchString(err.Error())
}

// holdBulkLane holds every process's bulk lane for delay from now, or keeps
// a later hold. A hold that cannot be written is logged: the row is still
// put back.
func (m *mailerImpl) holdBulkLane(ctx context.Context, logger *zerolog.Logger, delay time.Duration, reason string) {
	at, err := m.budgetNow(ctx, m.db.Queries)
	var until time.Time
	if err == nil {
		until, err = m.db.HoldBulkLane(ctx, database.HoldBulkLaneParams{HeldUntil: at.Add(delay), Reason: reason})
	}
	if err != nil {
		logger.Err(err).Dur("hold", delay).Msg("The relay throttled the account and the bulk lane's hold could not be written")
		return
	}
	logger.Warn().Str("reason", reason).Time("held_until", until).Str("event", "bulk_lane_throttled").
		Msg("The relay throttled the account: the bulk lane waits")
}

// bulkLaneHeld says the bulk lane is held at at: a throttling reply's hold
// has not run out. With the hold off it never is.
func (m *mailerImpl) bulkLaneHeld(ctx context.Context, q *database.Queries, at time.Time) (bool, error) {
	if m.guard.ThrottlePause <= 0 {
		return false, nil
	}
	hold, err := q.BulkLaneHeldUntil(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hold.HeldUntil.After(at), nil
}

// deferThrottled handles a throttling reply to job: it holds the bulk lane
// and, for a bulk row, puts it back to pending until the hold runs out, with
// no attempt counted, and says it did. A system row is left to the usual
// retry (false), and so is a bulk row throttled maxRowThrottles times already,
// which holds nothing.
func (m *mailerImpl) deferThrottled(ctx context.Context, logger *zerolog.Logger, job database.MailQueue, sendErr error) bool {
	if m.guard.ThrottlePause <= 0 || !throttled(sendErr) {
		return false
	}
	bulk := Priority(job.Priority) <= PriorityBulk
	if bulk && job.Throttles >= maxRowThrottles {
		logger.Warn().Str("job_id", job.ID.String()).Int("throttles", job.Throttles).
			Msg("Row throttled too many times: retried as any other failure, the bulk lane not held")
		return false
	}
	delay := m.guard.ThrottlePause
	var later interface{ RetryAfter() time.Duration }
	if errors.As(sendErr, &later) && later.RetryAfter() > delay {
		delay = later.RetryAfter()
	}
	reason := sendErr.Error()
	m.holdBulkLane(ctx, logger, delay, reason)
	if !bulk {
		return false
	}
	n, err := m.db.DeferThrottledMailQueueItem(ctx, database.DeferThrottledMailQueueItemParams{
		ID:           job.ID,
		ClaimedBy:    m.claimant,
		ClaimedAt:    claimedAt(job),
		Error:        &reason,
		DelaySeconds: int(delay.Seconds()),
	})
	switch {
	case err != nil:
		logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to put a throttled row back")
	case n == 0:
		m.lostLease(logger, job, "throttled")
	default:
		logger.Info().Str("job_id", job.ID.String()).Dur("retry_in", delay).
			Msg("Throttled row put back to pending, no attempt counted")
	}
	return true
}

// countSendFailure counts a bulk row that failed for good at the relay
// against its send, which pauses once its failures pass the guard.
func (m *mailerImpl) countSendFailure(ctx context.Context, logger *zerolog.Logger, job database.MailQueue) {
	if Priority(job.Priority) > PriorityBulk {
		return
	}
	counted, err := m.db.CountTaskSendFailure(ctx, database.CountTaskSendFailureParams{
		TaskID:      job.TaskID,
		MinFailures: m.guard.FailureCount,
		MinPercent:  m.guard.FailurePercent,
	})
	if err != nil {
		logger.Err(err).Str("task_id", job.TaskID.String()).Msg("Failed to count a send's failure")
		return
	}
	if counted.Paused {
		if _, err := m.db.ParkPendingRows(ctx, job.TaskID); err != nil {
			logger.Err(err).Str("task_id", job.TaskID.String()).Msg("Failed to park a paused send's pending rows; the take hands them back one by one")
		}
		logger.Warn().Str("task_id", job.TaskID.String()).Int("send_failures", counted.SendFailures).
			Int("failure_pause_count", m.guard.FailureCount).Int("failure_pause_percent", m.guard.FailurePercent).
			Str("event", "send_paused_failure_rate").
			Msg("Send paused: its recipients failed past the threshold; resume or cancel it")
	}
}

// sweepCancelled writes failed the pending rows of cancelled sends that a
// retry or an expired lease put back to pending after the cancel.
func (m *mailerImpl) sweepCancelled(ctx context.Context) {
	n, err := m.db.CancelPendingRows(ctx, nil)
	if err != nil {
		m.logger.Err(err).Msg("Failed to sweep the pending rows of cancelled sends")
		return
	}
	if n > 0 {
		m.logger.Info().Int64("count", n).Msg("Cancelled sends' rows put back since written cancelled")
	}
}

// unparkResumed puts back due the parked rows of sends neither paused nor
// cancelled: a take that read a send paused can park its rows after an
// operator resumed it, and a failure rate pause can park them after the
// resume. Nothing else would ever take them.
func (m *mailerImpl) unparkResumed(ctx context.Context) {
	n, err := m.db.UnparkResumedRows(ctx)
	if err != nil {
		m.logger.Err(err).Msg("Failed to put back the parked rows of resumed sends")
		return
	}
	if n > 0 {
		m.logger.Info().Int64("count", n).Msg("Parked rows of sends no longer paused put back due")
	}
}

// skipHeldTasks hands back, of rows this process just took, those of a
// paused or a cancelled send: a paused send's go back parked (never due until
// it is resumed), a cancelled send's are written 'cancelled'. Such a row is
// one a retry or an expired lease put back to pending after the pause or the
// cancel; the take itself never reads them (parked rows are not due).
func (m *mailerImpl) skipHeldTasks(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []database.MailQueue) ([]database.MailQueue, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	seen := map[uuid.UUID]bool{}
	var tasks []uuid.UUID
	for _, row := range rows {
		if !seen[row.TaskID] {
			seen[row.TaskID] = true
			tasks = append(tasks, row.TaskID)
		}
	}
	held, err := q.GetHeldTasks(ctx, tasks)
	if err != nil || len(held) == 0 {
		return rows, err
	}
	cancelled := map[uuid.UUID]bool{}
	paused := map[uuid.UUID]bool{}
	for _, task := range held {
		if task.Cancelled {
			cancelled[task.ID] = true
		} else {
			paused[task.ID] = true
		}
	}
	var keep []database.MailQueue
	var park, cancel []uuid.UUID
	for _, row := range rows {
		switch {
		case cancelled[row.TaskID]:
			cancel = append(cancel, row.ID)
		case paused[row.TaskID]:
			park = append(park, row.ID)
		default:
			keep = append(keep, row)
		}
	}
	if len(park) > 0 {
		if _, err := q.ParkClaimedRows(ctx, database.ParkClaimedRowsParams{Ids: park, ClaimedBy: m.claimant}); err != nil {
			return nil, err
		}
	}
	if len(cancel) > 0 {
		if _, err := q.CancelClaimedRows(ctx, database.CancelClaimedRowsParams{Ids: cancel, ClaimedBy: m.claimant}); err != nil {
			return nil, err
		}
	}
	logger.Info().Int("parked", len(park)).Int("cancelled", len(cancel)).
		Msg("Rows of paused or cancelled sends taken: parked or written cancelled, not sent")
	return keep, nil
}
