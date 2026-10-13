package mailer

import (
	"context"
	"errors"
	"fmt"

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
	// After how many sends failing one after another a run stops and counts
	// none of its failures (5): the fault is SkyMail's, not the sends'.
	BreakerFailures int
}

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
	return o
}

// MaskResult counts what one run of MaskSensitiveContent did.
type MaskResult struct {
	// Sends masked, and the queue rows of theirs.
	Sends int
	Rows  int
	// Sends that could not be masked for certain, or failed MaxFailures times:
	// emptied, not kept.
	NotKept int
	// Sends looked at and found not sensitive.
	NotSensitive int
	// Sends whose masking failed in this run (each tried once).
	Failed int
	// The run stopped after BreakerFailures sends failed one after another,
	// and counted none of its failures.
	BreakerTripped bool
}

// maskSendHook, when set (tests), runs before each send is masked.
var maskSendHook func(uuid.UUID)

// MaskSensitiveContent looks at the sends still to be decided on
// (content_mask_due) that are done — none of their rows is still to go out,
// five minutes old — one send a transaction: a sensitive one has each row's
// mail replaced with its masked copy, RowsPerChunk rows at a time, and its
// variables with their masked values; one found not sensitive is not looked
// at again until its template changes. A send in which a credential is still
// found after masking is emptied instead. A send whose masking fails is
// counted and left for the next run, and emptied on its MaxFailures-th
// failure, so it never holds the others back and its raw mail is not kept.
// The raw mail is kept nowhere after.
func MaskSensitiveContent(ctx context.Context, db *database.Store, opts MaskOptions) (MaskResult, error) {
	opts = opts.withDefaults()
	var total MaskResult
	// The sends that failed in this run, not to be taken again in it; their
	// failures are counted once the run is over, unless the breaker trips.
	var failed []failedSend
	tried := []uuid.UUID{}
	inARow := 0
	sensitive := 0
	for sensitive < opts.MaxSends && total.NotSensitive < opts.NotSensitiveSends {
		if err := ctx.Err(); err != nil {
			return total, countFailures(ctx, db, failed, opts, &total, err)
		}
		var send *database.TakeSendToMaskRow
		var outcome sendOutcome
		err := db.InTx(ctx, func(q *database.Queries) error {
			sends, err := q.TakeSendToMask(ctx, tried)
			if err != nil || len(sends) == 0 {
				return err
			}
			send = &sends[0]
			outcome, err = maskSendSafely(ctx, q, *send, opts.RowsPerChunk)
			return err
		})
		if send == nil {
			return total, countFailures(ctx, db, failed, opts, &total, err)
		}
		if err != nil {
			if ctx.Err() != nil {
				return total, countFailures(ctx, db, failed, opts, &total, ctx.Err())
			}
			sensitive++
			total.Failed++
			tried = append(tried, send.ID)
			failed = append(failed, failedSend{id: send.ID, cause: err})
			inARow++
			if inARow >= opts.BreakerFailures {
				total.BreakerTripped = true
				log.Error().Err(err).Int("sends", inARow).
					Msg("masking failed for several sends in a row: a fault across SkyMail; the run stops and counts none of its failures")
				return total, nil
			}
			continue
		}
		inARow = 0
		switch outcome.kind {
		case outcomeMasked:
			sensitive++
			total.Sends++
			total.Rows += outcome.rows
		case outcomeNotKept:
			sensitive++
			total.NotKept++
		case outcomeNotSensitive:
			total.NotSensitive++
		}
	}
	return total, countFailures(ctx, db, failed, opts, &total, nil)
}

type failedSend struct {
	id    uuid.UUID
	cause error
}

// countFailures counts the run's failures, each in a transaction of its own,
// and returns err.
func countFailures(ctx context.Context, db *database.Store, failed []failedSend, opts MaskOptions, total *MaskResult, err error) error {
	for _, f := range failed {
		if failSend(context.WithoutCancel(ctx), db, f.id, opts.MaxFailures, f.cause) {
			total.NotKept++
		}
	}
	return err
}

// maskSendSafely is maskSend with a panic turned into the send's failure: one
// mail that breaks the masking never brings SkyMail down, nor the sweep that
// runs as it starts.
func maskSendSafely(ctx context.Context, q *database.Queries, send database.TakeSendToMaskRow, rowsPerChunk int) (outcome sendOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("masking the send panicked: %v", r)
		}
	}()
	if maskSendHook != nil {
		maskSendHook(send.ID)
	}
	return maskSend(ctx, q, send, rowsPerChunk)
}

// failSend counts a failed masking of a send; on its maxFailures-th it empties
// the send's content and says so. It logs the send and the count, never its
// content.
func failSend(ctx context.Context, db *database.Store, id uuid.UUID, maxFailures int, cause error) bool {
	failures, err := db.RecordMaskFailure(ctx, id)
	if err != nil {
		log.Error().Err(err).Str("task_id", id.String()).Msg("could not count a failed masking of a sensitive send")
		return false
	}
	if int(failures) < maxFailures {
		log.Warn().Err(cause).Str("task_id", id.String()).Int16("failures", failures).
			Msg("masking a sensitive send failed; it is tried again on the next sweep")
		return false
	}
	if err := db.ClearSendContent(ctx, id); err != nil {
		log.Error().Err(err).Str("task_id", id.String()).Msg("could not empty a sensitive send that failed to mask")
		return false
	}
	log.Error().Err(cause).Str("task_id", id.String()).Int16("failures", failures).
		Msg("masking a sensitive send failed too many times: its content was emptied (not kept)")
	return true
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
	masker := NewMasker(send.BodyVariables)
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
			mail, err := masker.Mail(sent, RecipientInfo{FullName: row.RecipientFullName, Email: row.RecipientEmail})
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
