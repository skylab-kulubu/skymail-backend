package mailer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// MaskOptions bound one run of MaskSensitiveContent; a zero field is its
// default.
type MaskOptions struct {
	// How many sensitive sends one run masks or tries, at most (200). Sends
	// found not sensitive do not count; NotSensitiveSends bounds them (5000).
	MaxSends          int
	NotSensitiveSends int
	// How many rows of a send are read, masked and written at a time (500).
	RowsPerChunk int
	// After how many failures in a row a send's content is emptied (3).
	MaxFailures int
	// After how many sends failing one after another a run stops (5).
	BreakerFailures int
	// How long a send whose masking failed waits before it is tried again,
	// doubled for each failure it has had, a day at most (5 minutes).
	RetryAfter time.Duration
	// How long a sensitive send's masking may keep failing, its failures
	// counted or not, before its content is emptied (7 days).
	Deadline time.Duration
}

// MaskAttentionEvent is the event field of the line the sweep writes, at error
// level, when it empties a sensitive send it could not mask: reason
// "deadline" (failing for longer than MaskOptions.Deadline) or "failures"
// (MaxFailures counted failures). The line carries the send's id, never its
// content; count it in the service's logs (README, Sent content).
const MaskAttentionEvent = "sensitive_content_mask_attention"

// MaskFailedEvent is the event field of the warning the sweep writes each
// time masking a sensitive send fails and the send is put back.
const MaskFailedEvent = "sensitive_content_mask_failed"

func (o MaskOptions) withDefaults() MaskOptions {
	if o.MaxSends <= 0 {
		o.MaxSends = 200
	}
	if o.RowsPerChunk <= 0 {
		o.RowsPerChunk = 500
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = 3
	}
	if o.NotSensitiveSends <= 0 {
		o.NotSensitiveSends = 5000
	}
	if o.BreakerFailures <= 0 {
		o.BreakerFailures = 5
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 5 * time.Minute
	}
	if o.Deadline <= 0 {
		o.Deadline = 7 * 24 * time.Hour
	}
	return o
}

// MaskResult counts what one run of MaskSensitiveContent did.
type MaskResult struct {
	// Sends masked, and the queue rows of theirs.
	Sends int
	Rows  int
	// Sends that could not be masked for certain, failed MaxFailures times,
	// or kept failing past the Deadline: emptied, not kept.
	NotKept int
	// Of NotKept, the sends emptied for failing past the Deadline.
	PastDeadline int
	// Sends looked at and found not sensitive.
	NotSensitive int
	// Sends whose masking failed in this run (each tried once).
	Failed int
	// The run stopped after BreakerFailures sends failed one after another.
	BreakerTripped bool
	// The run masked no send, so its failures — maybe SkyMail's, not the
	// sends' — were not counted; they wait for a run that masks one.
	FailuresUnconfirmed bool
}

// maskSendHook, when set (tests), runs before each send is masked.
var maskSendHook func(uuid.UUID)

// MaskSensitiveContent looks at the sends still to be decided on
// (content_mask_due) that are done — none of their rows is still to go out,
// five minutes old — one send a transaction: a sensitive one has each row's
// mail replaced with its masked copy, RowsPerChunk rows at a time, and its
// variables with their masked values; one found not sensitive is not looked
// at again until its template changes. A send in which a credential is still
// found after masking is emptied instead. The raw mail is kept nowhere after.
//
// A send whose masking fails (a panic included) is put back: tried again
// after RetryAfter, doubled for each failure, behind the sends that never
// failed, so broken sends at the head of the queue hold no other back. Its
// failure is counted only when a run masks some other send — the fault was
// the send's — and only the failures of that run: one from a run that masked
// none is never counted, as the fault may have been SkyMail's, so a fault
// across SkyMail empties nothing once it is over. On the MaxFailures-th
// counted failure the send is emptied (not kept). A run stops after
// BreakerFailures sends fail one after another. Whatever was counted, a
// sensitive send still failing Deadline after its first failure is emptied at
// the end of every run, so a queue of broken sends alone keeps no raw mail
// for long. Each send emptied for its failures is logged as
// MaskAttentionEvent.
func MaskSensitiveContent(ctx context.Context, db *database.Store, opts MaskOptions) (MaskResult, error) {
	opts = opts.withDefaults()
	var total MaskResult
	tried := []uuid.UUID{}
	failed := []uuid.UUID{}
	inARow, sensitive, masked := 0, 0, 0
	var runErr error
	for sensitive < opts.MaxSends && total.NotSensitive < opts.NotSensitiveSends {
		if err := ctx.Err(); err != nil {
			runErr = err
			break
		}
		var send *database.TakeSendToMaskRow
		var outcome sendOutcome
		err := db.InTx(ctx, func(q *database.Queries) error {
			return safely(func() error {
				sends, err := q.TakeSendToMask(ctx, tried)
				if err != nil || len(sends) == 0 {
					return err
				}
				send = &sends[0]
				outcome, err = maskSendSafely(ctx, q, *send, opts.RowsPerChunk)
				return err
			})
		})
		if send == nil {
			runErr = err
			break
		}
		tried = append(tried, send.ID)
		if err != nil {
			if ctx.Err() != nil {
				runErr = ctx.Err()
				break
			}
			sensitive++
			total.Failed++
			failed = append(failed, send.ID)
			deferFailure(ctx, db, send.ID, opts.RetryAfter, err)
			inARow++
			if inARow >= opts.BreakerFailures {
				total.BreakerTripped = true
				log.Error().Err(err).Int("sends", inARow).
					Msg("masking failed for several sends in a row: the run stops; the sends are tried again later")
				break
			}
			continue
		}
		inARow = 0
		switch outcome.kind {
		case outcomeMasked:
			sensitive++
			masked++
			total.Sends++
			total.Rows += outcome.rows
		case outcomeNotKept:
			sensitive++
			masked++
			total.NotKept++
		case outcomeNotSensitive:
			total.NotSensitive++
		}
	}
	switch {
	case masked > 0:
		total.NotKept += confirmFailures(context.WithoutCancel(ctx), db, failed, opts.MaxFailures)
	case total.Failed > 0:
		total.FailuresUnconfirmed = true
		log.Error().Int("failed", total.Failed).
			Msg("masking failed and no send was masked: maybe a fault across SkyMail; this run's failures are not counted")
	}
	if ctx.Err() == nil {
		emptied := clearPastDeadline(ctx, db, opts.Deadline)
		total.PastDeadline += emptied
		total.NotKept += emptied
	}
	return total, runErr
}

// deadlineBatch is how many sends one statement of clearPastDeadline empties.
const deadlineBatch = 20

// clearPastDeadline empties the sensitive sends whose masking has kept
// failing for longer than deadline, logging each as MaskAttentionEvent, and
// returns how many it emptied.
func clearPastDeadline(ctx context.Context, db *database.Store, deadline time.Duration) int {
	emptied := 0
	err := safely(func() error {
		for {
			sends, err := db.ClearSendsPastMaskDeadline(ctx, database.ClearSendsPastMaskDeadlineParams{
				DeadlineSeconds: deadline.Seconds(), BatchSize: deadlineBatch,
			})
			if err != nil {
				return err
			}
			for _, send := range sends {
				emptied++
				log.Error().Str("event", MaskAttentionEvent).Str("reason", "deadline").
					Str("task_id", send.ID.String()).Time("failing_since", send.FailingSince).
					Msg("masking a sensitive send kept failing past its deadline: its content was emptied (not kept)")
			}
			if len(sends) < deadlineBatch {
				return nil
			}
		}
	})
	if err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("could not empty the sensitive sends failing to mask past their deadline")
	}
	return emptied
}

// safely runs fn with a panic turned into its error: one mail that breaks the
// masking never brings SkyMail down, nor the sweep that runs as it starts.
func safely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("masking panicked: %v", r)
		}
	}()
	return fn()
}

// deferFailure puts a send whose masking failed back, uncounted, to be tried
// again later. It logs the send, never its content.
func deferFailure(ctx context.Context, db *database.Store, id uuid.UUID, retryAfter time.Duration, cause error) {
	err := safely(func() error {
		return db.DeferMaskFailure(context.WithoutCancel(ctx), database.DeferMaskFailureParams{
			ID: id, BaseSeconds: retryAfter.Seconds(),
		})
	})
	if err != nil {
		log.Error().Err(err).Str("task_id", id.String()).Msg("could not put back a sensitive send that failed to mask")
		return
	}
	log.Warn().Err(cause).Str("event", MaskFailedEvent).Str("task_id", id.String()).
		Msg("masking a sensitive send failed; it is tried again later")
}

// confirmFailures counts the failure of each send that failed in this run,
// now that it has masked a send, and empties each send that reaches
// maxFailures. It returns how many it emptied.
func confirmFailures(ctx context.Context, db *database.Store, failed []uuid.UUID, maxFailures int) int {
	if len(failed) == 0 {
		return 0
	}
	emptied := 0
	err := safely(func() error {
		counted, err := db.ConfirmMaskFailures(ctx, failed)
		if err != nil {
			return err
		}
		for _, send := range counted {
			if int(send.ContentMaskFailures) < maxFailures {
				continue
			}
			if err := db.ClearSendContent(ctx, send.ID); err != nil {
				log.Error().Err(err).Str("task_id", send.ID.String()).Msg("could not empty a sensitive send that failed to mask")
				continue
			}
			emptied++
			log.Error().Str("event", MaskAttentionEvent).Str("reason", "failures").
				Str("task_id", send.ID.String()).Int16("failures", send.ContentMaskFailures).
				Msg("masking a sensitive send failed too many times: its content was emptied (not kept)")
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Msg("could not count the failed maskings of sensitive sends")
	}
	return emptied
}

// maskSendSafely is maskSend with a panic turned into the send's failure.
func maskSendSafely(ctx context.Context, q *database.Queries, send database.TakeSendToMaskRow, rowsPerChunk int) (outcome sendOutcome, err error) {
	err = safely(func() error {
		if maskSendHook != nil {
			maskSendHook(send.ID)
		}
		var err error
		outcome, err = maskSend(ctx, q, send, rowsPerChunk)
		return err
	})
	return outcome, err
}

type outcomeKind int

const (
	outcomeMasked outcomeKind = iota
	outcomeNotKept
	outcomeNotSensitive
)

type sendOutcome struct {
	kind outcomeKind
	rows int
}

// maskSend decides on one send, in the caller's transaction.
func maskSend(ctx context.Context, q *database.Queries, send database.TakeSendToMaskRow, rowsPerChunk int) (sendOutcome, error) {
	if !send.Sensitive {
		return sendOutcome{kind: outcomeNotSensitive}, q.MarkSendNotDueForMasking(ctx, send.ID)
	}
	masker := NewMaskerListing(send.BodyVariables, send.SensitiveVariables)
	if send.TemplateID != nil {
		parts, err := q.ListTemplatePartsInForceAt(ctx, database.ListTemplatePartsInForceAtParams{
			TemplateID: *send.TemplateID, SentAt: send.CreatedAt,
		})
		if err != nil {
			return sendOutcome{}, err
		}
		candidates := make([]TemplateParts, 0, len(parts))
		for _, part := range parts {
			candidates = append(candidates, TemplateParts{Subject: part.Subject, PlainText: part.PlainTextContent, HTML: part.HtmlContent})
		}
		masker.UseTemplates(candidates)
	}

	method := MaskedByVariables
	masked := 0
	after := uuid.Nil
	for {
		rows, err := q.ListQueueRowsToMask(ctx, database.ListQueueRowsToMaskParams{
			TaskID: send.ID, After: after, RowLimit: int32(rowsPerChunk),
		})
		if err != nil {
			return sendOutcome{}, err
		}
		update := database.SetMaskedQueueContentParams{TaskID: send.ID}
		for _, row := range rows {
			sent := Rendered{Subject: row.Subject, PlainText: row.Body}
			if row.BodyHtml != nil {
				sent.HTML = *row.BodyHtml
			}
			recipient := RecipientInfo{FullName: row.RecipientFullName, Email: row.RecipientEmail}
			if row.ListUnsubscribeUrl != nil {
				recipient.UnsubscribeURL = *row.ListUnsubscribeUrl
			}
			mail, err := masker.Mail(sent, recipient)
			if errors.Is(err, ErrCannotMask) {
				// Rows of this send masked in earlier chunks are emptied too.
				return sendOutcome{kind: outcomeNotKept}, q.ClearSendContent(ctx, send.ID)
			}
			if err != nil {
				return sendOutcome{}, err
			}
			if mail.Method == MaskedByPatterns {
				method = MaskedByPatterns
			}
			update.Ids = append(update.Ids, row.ID)
			update.Subjects = append(update.Subjects, mail.Subject)
			update.Bodies = append(update.Bodies, mail.PlainText)
			update.Htmls = append(update.Htmls, mail.HTML)
		}
		if len(update.Ids) > 0 {
			if err := q.SetMaskedQueueContent(ctx, update); err != nil {
				return sendOutcome{}, err
			}
			masked += len(update.Ids)
		}
		if len(rows) < rowsPerChunk {
			break
		}
		after = rows[len(rows)-1].ID
	}
	by := string(method)
	if err := q.MarkSendMasked(ctx, database.MarkSendMaskedParams{
		ID:              send.ID,
		BodyVariables:   masker.MaskedVariables(),
		MaskedBy:        &by,
		MaskedVariables: masker.Variables(),
	}); err != nil {
		return sendOutcome{}, err
	}
	return sendOutcome{kind: outcomeMasked, rows: masked}, nil
}
