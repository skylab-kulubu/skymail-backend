package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Open and click tracking's withdrawal and retention (hub ticket
// iletisim-servisi 14c; design note 07 §4.7; docs/data-lifecycle.md, open
// and click tracking).
//
// Lock order. A writer of an opening or a click locks the recipient's queue
// row (LockTrackingRecipient), then inserts the event, whose foreign key
// takes the link's key-share lock. Everything here that changes tracking
// takes the queue rows it touches first, by id, then the links, then the
// events — Account erasure's order (newEraser) — so it waits for an event in
// flight and deletes it, and no click ever waits behind a link lock held by
// something that waits for its queue row.

// TrackingWithdrawal counts what one withdrawal changed.
type TrackingWithdrawal struct {
	TokensClosed  int64
	EventsDeleted int64
}

// WithdrawTracking stops tracking a person (core's withdrawal: they turned
// it off on my.). Every token of mail to their subject closes — mail already
// sent and mail still queued, which then goes out without the pixel — and
// their openings and clicks are deleted. Their old mail's links still
// redirect; the counts without people already kept stay. A repeat changes
// nothing. Mail queued later asks core again and is not tracked.
func (s *Store) WithdrawTracking(ctx context.Context, subject string) (TrackingWithdrawal, error) {
	var done TrackingWithdrawal
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		// A row held longer than this (masking, an erasure) fails the call
		// with lock_not_available; the route answers 503 and core retries.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+trackingLockTimeout+`'`); err != nil {
			return err
		}
		// idx_mail_queue_tracking_subject holds exactly these rows.
		if _, err := tx.Exec(ctx, `
			SELECT q.id FROM mail_queue q
			WHERE q.recipient_sub = $1::uuid AND q.tracking_token IS NOT NULL
			ORDER BY q.id FOR UPDATE`, subject); err != nil {
			return fmt.Errorf("lock the person's tracked rows: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE mail_queue q SET tracking_disabled_at = now()
			WHERE q.recipient_sub = $1::uuid AND q.tracking_token IS NOT NULL AND q.tracking_disabled_at IS NULL`, subject)
		if err != nil {
			return fmt.Errorf("close the person's tokens: %w", err)
		}
		done.TokensClosed = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `
			DELETE FROM mail_tracking_events e USING mail_queue q
			WHERE e.queue_id = q.id AND q.recipient_sub = $1::uuid AND q.tracking_token IS NOT NULL`, subject)
		if err != nil {
			return fmt.Errorf("delete the person's events: %w", err)
		}
		done.EventsDeleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return TrackingWithdrawal{}, fmt.Errorf("tracking withdrawal: %w", err)
	}
	return done, nil
}

// trackingLockTimeout bounds how long a withdrawal, or one send's
// retention transaction, waits for a row another transaction holds.
const trackingLockTimeout = "5s"

// TrackingRetentionLockName names the session advisory lock one run holds:
// one run at a time across replicas.
const TrackingRetentionLockName = "skymail-backend tracking retention"

// TrackingRetentionBrakeMaxRows is the brake: an apply that would close
// more tokens, delete more events or empty more links than this in one run
// changes nothing, and says so (refused_large). A wrong clock or a wrong
// rule can then never empty the tables at once. purge, the roll-back asked
// for by hand, has no brake.
const TrackingRetentionBrakeMaxRows = tracking.DefaultRetentionMaxRows

// ErrTrackingRetentionLocked is returned when another run holds the lock.
var ErrTrackingRetentionLocked = errors.New("tracking retention: another run holds the lock")

// ErrTrackingRetentionNotDue is returned by a run with OnlyIfDue that finds,
// holding the lock, that a run of its mode is not due.
var ErrTrackingRetentionNotDue = errors.New("tracking retention: not due")

// TrackingRetentionCounts is what a run's rules changed (apply, purge) or
// would change (a dry run, a refused apply).
type TrackingRetentionCounts struct {
	// Sends looked at.
	Sends int64 `json:"sends"`
	// Counts without people kept.
	TotalsWritten int64 `json:"totals_written"`
	// Counts of sends with fewer than TotalsMinRecipients tracked
	// recipients, a year after the send: deleted.
	TotalsDeleted int64 `json:"totals_deleted"`
	TokensClosed  int64 `json:"tokens_closed"`
	EventsDeleted int64 `json:"events_deleted"`
	// Links a year old: url emptied, expired.
	LinksExpired int64 `json:"links_expired"`
	// Links of a send masked or sensitive since it went out (D7): url
	// emptied, erased.
	LinksErased int64 `json:"links_erased"`
	// Sends all of whose mail is past EventRetention: tracking_closed_at.
	SendsClosed int64 `json:"sends_closed"`
	// Sends whose transaction failed (the run is then partial).
	SendsFailed int64 `json:"sends_failed"`
}

func (c *TrackingRetentionCounts) add(o TrackingRetentionCounts) {
	c.Sends += o.Sends
	c.TotalsWritten += o.TotalsWritten
	c.TotalsDeleted += o.TotalsDeleted
	c.TokensClosed += o.TokensClosed
	c.EventsDeleted += o.EventsDeleted
	c.LinksExpired += o.LinksExpired
	c.LinksErased += o.LinksErased
	c.SendsClosed += o.SendsClosed
	c.SendsFailed += o.SendsFailed
}

// TrackingRetentionOptions say how to run.
type TrackingRetentionOptions struct {
	// Mode is dry-run, apply or purge.
	Mode tracking.RetentionMode
	// BrakeMaxRows replaces TrackingRetentionBrakeMaxRows when positive
	// (SKYMAIL_TRACKING_RETENTION_MAX_ROWS).
	BrakeMaxRows int64
	// OnlyIfDue makes the run check, once it holds the lock, that a run of
	// its mode is due (TrackingRetentionDue), and do nothing otherwise
	// (ErrTrackingRetentionNotDue): the schedule's runs.
	OnlyIfDue bool
}

// TrackingRetentionReport is what a run did; its receipt is the
// tracking_retention_runs row RunID.
type TrackingRetentionReport struct {
	RunID int64
	Mode  tracking.RetentionMode
	// ok; partial (a send failed); refused_large (apply: the brake refused,
	// nothing changed); would_refuse_large (a dry run whose apply the brake
	// would refuse); failed.
	Status string
	// Mail sent at or before this is past EventRetention.
	EventsCutoff time.Time
	Counts       TrackingRetentionCounts
}

// TrackingRetentionDue reports whether a run of mode is due: none ended ok,
// partial or refused in the last 23 hours. It reads the receipts, so a
// restart or a release never runs it twice in a day; a failed run does not
// count. Changing the mode makes a run of the new mode due at once.
func (s *Store) TrackingRetentionDue(ctx context.Context, mode tracking.RetentionMode) (bool, error) {
	var due bool
	err := s.Conn.QueryRow(ctx, trackingRetentionDueSQL, string(mode)).Scan(&due)
	return due, err
}

const trackingRetentionDueSQL = `
	SELECT NOT EXISTS (SELECT 1 FROM tracking_retention_runs
	                   WHERE mode = $1 AND status IN ('ok', 'partial', 'refused_large', 'would_refuse_large')
	                     AND started_at > now() - interval '23 hours')`

// RunTrackingRetention runs the retention rules once (design note 07 §4.7;
// KVKK notice s6, 2026-10-23: 90 days), holding the advisory lock, and
// writes its receipt.
//
// For each send with something due, in one transaction of its own, after
// locking the send's tracked queue rows by id:
//
//  1. the send's counts without people are kept, from all its events, when
//     one of its recipients' mail went out EventRetention ago or more and
//     it has none kept yet (a send with fewer than TotalsMinRecipients
//     tracked rows only within a year of its creation);
//  2. those recipients' tokens close;
//  3. its links a year old are emptied (expired), and every link of a send
//     masked or sensitive now (D7) is emptied (erased);
//  4. those recipients' events are deleted: nothing ties an opening or a
//     click to a person any more, and nothing can tie it back;
//  5. a send none of whose mail is still to go out or within EventRetention
//     closes (tracking_closed_at);
//  6. the counts of a send with fewer than TotalsMinRecipients tracked
//     rows are deleted a year after the send was created, with its content.
//
// A dry run runs exactly these statements and rolls each transaction back:
// its counts are what apply changes. Apply first runs the dry run; when a
// count passes the brake it changes nothing (refused_large). purge closes
// every token and deletes every event in one transaction, without a brake.
func (s *Store) RunTrackingRetention(ctx context.Context, opts TrackingRetentionOptions) (*TrackingRetentionReport, error) {
	switch opts.Mode {
	case tracking.RetentionDryRun, tracking.RetentionApply, tracking.RetentionPurge:
	default:
		return nil, fmt.Errorf("tracking retention: mode %q runs nothing", opts.Mode)
	}
	brake := opts.BrakeMaxRows
	if brake <= 0 {
		brake = TrackingRetentionBrakeMaxRows
	}

	conn, err := s.Conn.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracking retention: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, TrackingRetentionLockName).Scan(&locked); err != nil {
		return nil, fmt.Errorf("tracking retention lock: %w", err)
	}
	if !locked {
		return nil, ErrTrackingRetentionLocked
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, TrackingRetentionLockName)
	}()

	if opts.OnlyIfDue {
		var due bool
		if err := conn.QueryRow(ctx, trackingRetentionDueSQL, string(opts.Mode)).Scan(&due); err != nil {
			return nil, fmt.Errorf("tracking retention due: %w", err)
		}
		if !due {
			return nil, ErrTrackingRetentionNotDue
		}
	}

	report := &TrackingRetentionReport{Mode: opts.Mode}
	var now time.Time
	if err := s.Conn.QueryRow(ctx, `
		INSERT INTO tracking_retention_runs (mode, events_cutoff)
		VALUES ($1, now() - make_interval(days => $2::int))
		RETURNING id, now(), events_cutoff`, string(opts.Mode), tracking.EventRetentionDays).Scan(&report.RunID, &now, &report.EventsCutoff); err != nil {
		return nil, fmt.Errorf("tracking retention receipt: %w", err)
	}

	refused := false
	runErr := func() error {
		if opts.Mode == tracking.RetentionPurge {
			counts, err := s.purgeTracking(ctx)
			report.Counts = counts
			return err
		}
		sends, err := s.trackingRetentionSends(ctx, now)
		if err != nil {
			return err
		}
		// The dry run, which apply runs first.
		counts, err := s.trackingRetentionPass(ctx, sends, now, false)
		report.Counts = counts
		if err != nil {
			return err
		}
		// The brake decides on the dry run's counts, and only those: an
		// apply that went ahead is never reported refused.
		refused = counts.refusedBy(brake)
		if opts.Mode == tracking.RetentionApply && !refused {
			report.Counts, err = s.trackingRetentionPass(ctx, sends, now, true)
		}
		return err
	}()

	switch {
	case runErr != nil:
		report.Status = "failed"
	case refused && opts.Mode == tracking.RetentionApply:
		report.Status = "refused_large"
	case refused:
		report.Status = "would_refuse_large"
	case report.Counts.SendsFailed > 0:
		report.Status = "partial"
	default:
		report.Status = "ok"
	}
	counts, err := json.Marshal(report.Counts)
	if err != nil {
		return nil, err
	}
	if _, err := s.Conn.Exec(context.WithoutCancel(ctx), `
		UPDATE tracking_retention_runs SET status = $2, finished_at = clock_timestamp(), counts = $3::jsonb WHERE id = $1`,
		report.RunID, report.Status, string(counts)); err != nil && runErr == nil {
		runErr = fmt.Errorf("tracking retention receipt: %w", err)
	}
	if runErr != nil {
		return report, fmt.Errorf("tracking retention: %w", runErr)
	}
	return report, nil
}

func (c TrackingRetentionCounts) refusedBy(brake int64) bool {
	return c.TokensClosed > brake || c.EventsDeleted > brake || c.LinksExpired+c.LinksErased > brake || c.TotalsDeleted > brake
}

// trackingRetentionSends lists the sends with something due, oldest first:
// tracking still open and created EventRetention ago or more (mail of
// theirs can be past it), a link due (a year old, or of a send masked or
// sensitive now), or the counts of a send with fewer than
// TotalsMinRecipients tracked recipients created a year ago or more.
func (s *Store) trackingRetentionSends(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := s.Conn.Query(ctx, `
		SELECT id FROM (
		    SELECT t.id, t.created_at FROM mail_tasks t
		    WHERE t.tracking_requested AND t.tracking_closed_at IS NULL
		      AND t.created_at <= @now::timestamptz - make_interval(days => @days::int)
		    UNION
		    SELECT t.id, t.created_at FROM mail_task_links l JOIN mail_tasks t ON t.id = l.task_id
		    WHERE l.url IS NOT NULL
		      AND (l.created_at <= @now::timestamptz - interval '1 year'
		           OR t.content_masked_at IS NOT NULL OR mail_task_content_sensitive(t.id))
		    UNION
		    SELECT t.id, t.created_at FROM mail_task_tracking_totals tt JOIN mail_tasks t ON t.id = tt.task_id
		    WHERE tt.tracked_recipients < @min::int AND t.created_at <= @now::timestamptz - interval '1 year'
		) due
		ORDER BY created_at, id`, pgx.NamedArgs{"now": now, "days": tracking.EventRetentionDays, "min": tracking.TotalsMinRecipients})
	if err != nil {
		return nil, fmt.Errorf("sends due: %w", err)
	}
	sends, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("sends due: %w", err)
	}
	return sends, nil
}

// trackingRetentionPass runs the rules on each send, committing each send's
// transaction when commit is true and rolling it back otherwise. A send that
// fails is counted and left for the next run; the others go on.
func (s *Store) trackingRetentionPass(ctx context.Context, sends []uuid.UUID, now time.Time, commit bool) (TrackingRetentionCounts, error) {
	var total TrackingRetentionCounts
	for _, task := range sends {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		counts, err := s.trackingRetentionSend(ctx, task, now, commit)
		if err != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			total.SendsFailed++
			continue
		}
		total.add(counts)
	}
	return total, nil
}

// trackingRetentionRules are one send's statements, in lock order: queue
// rows, then links, then events. @task is the send, @now the run's time,
// @days EventRetentionDays, @min TotalsMinRecipients.
var trackingRetentionRules = []struct {
	sql   string
	count func(*TrackingRetentionCounts, int64)
}{
	{
		// Before anything else: the tracked rows of the send, by id.
		sql: `SELECT q.id FROM mail_queue q
		      WHERE q.task_id = @task AND q.tracking_token IS NOT NULL
		      ORDER BY q.id FOR UPDATE`,
	},
	{
		// 1. The counts without people (T4), as the live report has them,
		// from every event the send still has, before any goes. Every send
		// keeps them (KVKK notice s6); one with fewer than
		// TotalsMinRecipients tracked recipients only until a year after
		// its creation (rule 6), so a run late past it keeps none.
		//
		// A person counts as tracked only when their tracked mail went out
		// and their token is open (T4, orchestrator 2026-10-19): a token of
		// mail that never went out (not_delivered) or closed by a
		// withdrawal, an erasure or the flag (tracking_closed) could leave
		// fewer than ten behind the counts, and the reports show the counts
		// of fewer than ten to who may read the send's people only.
		sql: `WITH people AS (
		          SELECT q.tracking_token IS NOT NULL AND q.status = 'sent' AND q.tracking_disabled_at IS NULL AS tracked,
		                 CASE WHEN q.tracking_token IS NULL THEN COALESCE(q.tracking_skip_reason, 'not_requested')
		                      WHEN q.status <> 'sent' THEN 'not_delivered'
		                      ELSE 'tracking_closed' END AS reason
		          FROM mail_queue q WHERE q.task_id = @task
		      ), events AS (
		          SELECT e.queue_id, e.kind, e.link_id, e.automated_likely FROM mail_tracking_events e WHERE e.task_id = @task
		      ), links AS (
		          SELECT l.id, l.position, count(e.queue_id)::int AS clicks, count(DISTINCT e.queue_id)::int AS unique_clickers
		          FROM mail_task_links l LEFT JOIN events e ON e.link_id = l.id
		          WHERE l.task_id = @task GROUP BY l.id
		      )
		      INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, untracked_by_reason, unique_opens,
		                                             unique_likely_human_opens, unique_clickers, clicks_by_link, rolled_up_at)
		      SELECT @task::uuid,
		             (SELECT count(*) FROM people WHERE tracked),
		             COALESCE((SELECT jsonb_object_agg(reason, n)
		                       FROM (SELECT reason, count(*) AS n FROM people WHERE NOT tracked GROUP BY reason) r), '{}'::jsonb),
		             (SELECT count(DISTINCT queue_id) FROM events WHERE kind = 'open'),
		             (SELECT count(DISTINCT queue_id) FROM events WHERE NOT automated_likely),
		             (SELECT count(DISTINCT queue_id) FROM events WHERE kind = 'click'),
		             COALESCE((SELECT jsonb_agg(to_jsonb(links) ORDER BY links.position) FROM links), '[]'::jsonb),
		             @now::timestamptz
		      WHERE EXISTS (SELECT 1 FROM mail_tasks t WHERE t.id = @task AND t.tracking_closed_at IS NULL
		                      AND ((SELECT count(*) FROM people WHERE tracked) >= @min::int
		                           OR t.created_at > @now::timestamptz - interval '1 year'))
		        AND EXISTS (SELECT 1 FROM mail_queue q
		                    WHERE q.task_id = @task AND q.tracking_token IS NOT NULL AND q.status = 'sent'
		                      AND q.sent_at <= @now::timestamptz - make_interval(days => @days::int))
		      ON CONFLICT (task_id) DO NOTHING`,
		count: func(c *TrackingRetentionCounts, n int64) { c.TotalsWritten += n },
	},
	{
		// 2. The tokens of mail past EventRetention.
		sql: `UPDATE mail_queue q SET tracking_disabled_at = @now::timestamptz
		      WHERE q.task_id = @task AND q.tracking_token IS NOT NULL AND q.tracking_disabled_at IS NULL
		        AND q.status = 'sent' AND q.sent_at <= @now::timestamptz - make_interval(days => @days::int)`,
		count: func(c *TrackingRetentionCounts, n int64) { c.TokensClosed += n },
	},
	{
		// 3. Links a year old (T3; /t/c already answers 410 for them).
		sql: `UPDATE mail_task_links SET url = NULL, url_cleared_reason = 'expired'
		      WHERE task_id = @task AND url IS NOT NULL AND created_at <= @now::timestamptz - interval '1 year'`,
		count: func(c *TrackingRetentionCounts, n int64) { c.LinksExpired += n },
	},
	{
		// 3. D7: a send masked, or whose content is sensitive now (its
		// template was made so after it went out; a sensitive send is never
		// tracked to begin with): its links may carry what masking hides.
		sql: `UPDATE mail_task_links l SET url = NULL, url_cleared_reason = 'erased'
		      FROM mail_tasks t
		      WHERE l.task_id = @task AND t.id = l.task_id AND l.url IS NOT NULL
		        AND (t.content_masked_at IS NOT NULL OR mail_task_content_sensitive(t.id))`,
		count: func(c *TrackingRetentionCounts, n int64) { c.LinksErased += n },
	},
	{
		// 4. The events of mail past EventRetention.
		sql: `DELETE FROM mail_tracking_events e USING mail_queue q
		      WHERE e.queue_id = q.id AND q.task_id = @task
		        AND q.sent_at <= @now::timestamptz - make_interval(days => @days::int)`,
		count: func(c *TrackingRetentionCounts, n int64) { c.EventsDeleted += n },
	},
	{
		// 5. The send, once none of its mail is still to go out or within
		// EventRetention: its report is the counts without people from now
		// on.
		sql: `UPDATE mail_tasks t SET tracking_closed_at = @now::timestamptz
		      WHERE t.id = @task AND t.tracking_requested AND t.tracking_closed_at IS NULL
		        AND t.created_at <= @now::timestamptz - make_interval(days => @days::int)
		        AND NOT EXISTS (SELECT 1 FROM mail_queue q
		                        WHERE q.task_id = t.id
		                          AND (q.status IN ('pending', 'processing')
		                               OR (q.status = 'sent' AND q.sent_at > @now::timestamptz - make_interval(days => @days::int))))`,
		count: func(c *TrackingRetentionCounts, n int64) { c.SendsClosed += n },
	},
	{
		// 6. The counts of a send with fewer than TotalsMinRecipients
		// tracked recipients go a year after the send was created, as its
		// content does (KVKK notice s6).
		sql: `DELETE FROM mail_task_tracking_totals tt USING mail_tasks t
		      WHERE tt.task_id = @task AND t.id = tt.task_id AND tt.tracked_recipients < @min::int
		        AND t.created_at <= @now::timestamptz - interval '1 year'`,
		count: func(c *TrackingRetentionCounts, n int64) { c.TotalsDeleted += n },
	},
}

// trackingRetentionSend runs the rules on one send in one transaction.
func (s *Store) trackingRetentionSend(ctx context.Context, task uuid.UUID, now time.Time, commit bool) (TrackingRetentionCounts, error) {
	counts := TrackingRetentionCounts{Sends: 1}
	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		return counts, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// A click in flight holds a row for milliseconds; anything longer is
	// left for the next run rather than waited on.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+trackingLockTimeout+`'`); err != nil {
		return counts, err
	}
	args := pgx.NamedArgs{"task": task, "now": now, "days": tracking.EventRetentionDays, "min": tracking.TotalsMinRecipients}
	for _, rule := range trackingRetentionRules {
		tag, err := tx.Exec(ctx, rule.sql, args)
		if err != nil {
			return counts, err
		}
		if rule.count != nil {
			rule.count(&counts, tag.RowsAffected())
		}
	}
	if commit {
		return counts, tx.Commit(ctx)
	}
	return counts, nil
}

// purgeTracking closes every token and deletes every event, in one
// transaction, the tracked queue rows locked first.
func (s *Store) purgeTracking(ctx context.Context) (TrackingRetentionCounts, error) {
	var counts TrackingRetentionCounts
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT q.id FROM mail_queue q WHERE q.tracking_token IS NOT NULL ORDER BY q.id FOR UPDATE`); err != nil {
			return fmt.Errorf("lock tracked rows: %w", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE mail_queue SET tracking_disabled_at = now() WHERE tracking_token IS NOT NULL AND tracking_disabled_at IS NULL`)
		if err != nil {
			return fmt.Errorf("close tokens: %w", err)
		}
		counts.TokensClosed = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `DELETE FROM mail_tracking_events`)
		if err != nil {
			return fmt.Errorf("delete events: %w", err)
		}
		counts.EventsDeleted = tag.RowsAffected()
		return nil
	})
	return counts, err
}
