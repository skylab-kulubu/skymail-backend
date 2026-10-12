package mailer

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The send budget keeps every process together within the relay's limits:
// SKYMAIL_RATE_PER_MINUTE messages a minute and SKYMAIL_DAILY_RECIPIENT_BUDGET
// recipients a day (one queue row is one message to one recipient). The
// counters are rows in Postgres (mail_send_windows), which every replica
// reads and writes, so the limits hold however many processes send.
//
// Of each limit the bulk lane gets at most BulkSharePercent; the rest is the
// system lane's. A system row (a login code, a certificate) is always taken
// and only counted after it, so the budget never holds one back or drops one;
// what it takes past its share narrows what the bulk lane may take.
const (
	RatePerMinuteEnv        = "SKYMAIL_RATE_PER_MINUTE"
	DailyRecipientBudgetEnv = "SKYMAIL_DAILY_RECIPIENT_BUDGET"

	// DefaultRatePerMinute is under Exchange Online's 30 messages a minute
	// (smtp.office365.com, the production relay), with room for the lanes'
	// rounding and the windows' estimate.
	DefaultRatePerMinute = 25
	// DefaultDailyRecipientBudget is under Exchange Online's 10,000
	// recipients a day.
	DefaultDailyRecipientBudget = 9000
)

// RateBudget is the send budget. A limit of 0 is no limit; both 0 is no
// budget at all, the dispatcher taking rows as it did before the budget.
type RateBudget struct {
	PerMinute int
	PerDay    int
}

// Off says neither limit applies.
func (b RateBudget) Off() bool { return b.PerMinute <= 0 && b.PerDay <= 0 }

// RateBudgetFromEnv reads SKYMAIL_RATE_PER_MINUTE (unset: 25) and
// SKYMAIL_DAILY_RECIPIENT_BUDGET (unset: 9000), each a whole number, 0 for no
// limit. Anything else is an error that stops startup.
func RateBudgetFromEnv(getenv func(string) string) (RateBudget, error) {
	perMinute, err := limitFromEnv(getenv, RatePerMinuteEnv, DefaultRatePerMinute)
	if err != nil {
		return RateBudget{}, err
	}
	perDay, err := limitFromEnv(getenv, DailyRecipientBudgetEnv, DefaultDailyRecipientBudget)
	if err != nil {
		return RateBudget{}, err
	}
	return RateBudget{PerMinute: perMinute, PerDay: perDay}, nil
}

func limitFromEnv(getenv func(string) string, name string, unset int) (int, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return unset, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("%s must be a whole number, 0 for no limit, not %q", name, raw)
	}
	return limit, nil
}

// WithRateBudget sets the send budget. Without it the mailer has none.
func WithRateBudget(budget RateBudget) Option {
	return func(m *mailerImpl) { m.budget = budget }
}

// The budget's two windows, as mail_send_windows.span_seconds and the
// queries in query.sql have them.
const (
	minuteSpan = time.Minute
	daySpan    = 24 * time.Hour
	// keepWindows is how far back a window still counts: the current one
	// and the one before it, of the longest span.
	keepWindows = 2 * daySpan
)

// windowStart is the start of the window of span at holds: windows are
// aligned to the Unix epoch, a day's to UTC midnight.
func windowStart(at time.Time, span time.Duration) time.Time {
	seconds := int64(span / time.Second)
	unix := at.Unix()
	return time.Unix(unix-unix%seconds, 0).UTC()
}

// bulkRoom is how many bulk rows the windows leave room for at the instant
// at: the least room of the minute's and the day's.
func (b RateBudget) bulkRoom(at time.Time, counted []database.MailSendWindow) int {
	room := math.MaxInt
	for _, window := range []struct {
		span  time.Duration
		limit int
	}{{minuteSpan, b.PerMinute}, {daySpan, b.PerDay}} {
		if window.limit <= 0 {
			continue
		}
		current := windowStart(at, window.span)
		previous := current.Add(-window.span)
		var cur, prev [2]int // taken by lane: [0] bulk, [1] system
		for _, row := range counted {
			if time.Duration(row.SpanSeconds)*time.Second != window.span {
				continue
			}
			lane := 0
			if row.Lane > 0 {
				lane = 1
			}
			switch {
			case row.StartsAt.Equal(current):
				cur[lane] += row.Taken
			case row.StartsAt.Equal(previous):
				prev[lane] += row.Taken
			}
		}
		elapsed := float64(at.Sub(current)) / float64(window.span)
		room = min(room, windowRoom(window.limit, elapsed, cur[0], cur[1], prev[0], prev[1]))
	}
	return room
}

// windowRoom is the room one window of limit leaves the bulk lane, elapsed
// (0 to 1) of the way through it: the bulk lane's share less what it took,
// and the whole limit less what both lanes took. What the previous window took
// counts as much of it as still lies within a span of now (the sliding window
// estimate): the lanes are held to a limit over any span, not only within
// each aligned window, so a list never sends its share at the end of one
// minute and again at the start of the next.
func windowRoom(limit int, elapsed float64, curBulk, curSystem, prevBulk, prevSystem int) int {
	weight := 1 - elapsed
	bulk := float64(prevBulk)*weight + float64(curBulk)
	total := float64(prevBulk+prevSystem)*weight + float64(curBulk+curSystem)
	room := min(float64(bulkShare(limit))-bulk, float64(limit)-total)
	if room <= 0 {
		return 0
	}
	// The epsilon keeps 16.9999999 from float arithmetic a 17.
	return int(math.Floor(room + 1e-9))
}

// budgetNow is the instant the budget reads: the database's clock, which all
// processes share, or a test's.
func (m *mailerImpl) budgetNow(ctx context.Context, q *database.Queries) (time.Time, error) {
	if m.clock != nil {
		return m.clock(), nil
	}
	return q.DatabaseNow(ctx)
}

// claimBulk takes up to maxRows due bulk rows within the send budget. Under
// the budget's lock it reads the windows, takes no more rows than they leave
// room for and counts what it took, all in one transaction: another process
// takes its bulk rows only after, against this count. A failure takes
// nothing.
func (m *mailerImpl) claimBulk(ctx context.Context, logger *zerolog.Logger, maxRows int) []database.MailQueue {
	if m.budget.Off() {
		return m.claim(ctx, logger, false, maxRows)
	}
	var taken []database.MailQueue
	err := m.db.InTx(ctx, func(q *database.Queries) error {
		if err := q.LockSendBudget(ctx); err != nil {
			return err
		}
		at, err := m.budgetNow(ctx, q)
		if err != nil {
			return err
		}
		counted, err := q.SendWindowCounts(ctx, database.SendWindowCountsParams{
			MinuteSince: windowStart(at, minuteSpan).Add(-minuteSpan),
			DaySince:    windowStart(at, daySpan).Add(-daySpan),
		})
		if err != nil {
			return err
		}
		room := min(maxRows, m.budget.bulkRoom(at, counted))
		if room <= 0 {
			logger.Debug().Int("rate_per_minute", m.budget.PerMinute).Int("daily_recipient_budget", m.budget.PerDay).
				Msg("Bulk lane at its send budget: its rows wait for room")
			return nil
		}
		rows, err := q.ProcessBulkQueueItems(ctx, database.ProcessBulkQueueItemsParams{
			ClaimedBy: m.claimant,
			MaxRows:   room,
		})
		if err != nil || len(rows) == 0 {
			return err
		}
		if err := countSends(ctx, q, at, PriorityBulk, len(rows)); err != nil {
			return err
		}
		taken = rows
		return nil
	})
	if err != nil {
		logger.Err(err).Bool("system_lane", false).Msg("Failed to process queue items")
		return nil
	}
	return taken
}

// countSystem counts n system rows this process took, after it has handed
// them to the workers: a system row never waits on the budget. A count that
// fails is logged; the rows go all the same.
func (m *mailerImpl) countSystem(ctx context.Context, logger *zerolog.Logger, n int) {
	if n == 0 || m.budget.Off() {
		return
	}
	at, err := m.budgetNow(ctx, m.db.Queries)
	if err == nil {
		err = countSends(ctx, m.db.Queries, at, PrioritySystem, n)
	}
	if err != nil {
		logger.Warn().Err(err).Int("count", n).
			Msg("System mail taken without counting it in the send budget: the bulk lane may take more this window")
	}
}

func countSends(ctx context.Context, q *database.Queries, at time.Time, lane Priority, n int) error {
	return q.CountSends(ctx, database.CountSendsParams{
		MinuteStart: windowStart(at, minuteSpan),
		DayStart:    windowStart(at, daySpan),
		Lane:        int16(lane),
		Taken:       n,
	})
}

// pruneSendWindows drops the windows no budget reads any more.
func (m *mailerImpl) pruneSendWindows(ctx context.Context) {
	if m.budget.Off() {
		return
	}
	at, err := m.budgetNow(ctx, m.db.Queries)
	if err == nil {
		_, err = m.db.PruneSendWindows(ctx, at.Add(-keepWindows))
	}
	if err != nil {
		m.logger.Err(err).Msg("Failed to prune the send budget's old windows")
	}
}
