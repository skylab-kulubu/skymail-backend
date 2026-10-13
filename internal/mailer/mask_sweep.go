package mailer

import (
	"context"
	"errors"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// MaskResult counts what one run of MaskSensitiveContent did.
type MaskResult struct {
	// Sends masked, and the queue rows of theirs.
	Sends int
	Rows  int
	// Sends that could not be masked for certain: emptied, not kept.
	NotKept int
}

// MaskSensitiveContent masks the sensitive sends that are done (none of their
// rows is still to go out, five minutes old), batchSize at a time until a
// batch comes back short: each row's mail is replaced with its masked copy
// and the send's variables with their masked values, in one transaction a
// batch. A send in which a credential is still found after masking is
// emptied instead. The raw mail is kept nowhere after.
func MaskSensitiveContent(ctx context.Context, db *database.Store, batchSize int) (MaskResult, error) {
	var total MaskResult
	for {
		var batch MaskResult
		taken := 0
		err := db.InTx(ctx, func(q *database.Queries) error {
			sends, err := q.ListSendsToMask(ctx, batchSize)
			if err != nil {
				return err
			}
			taken = len(sends)
			for _, send := range sends {
				rows, kept, err := maskSend(ctx, q, send)
				if err != nil {
					return err
				}
				if kept {
					batch.Sends++
					batch.Rows += rows
				} else {
					batch.NotKept++
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total.Sends += batch.Sends
		total.Rows += batch.Rows
		total.NotKept += batch.NotKept
		if taken < batchSize {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// maskSend masks one send: the rows it masked, and whether it was kept.
func maskSend(ctx context.Context, q *database.Queries, send database.ListSendsToMaskRow) (int, bool, error) {
	var candidates []TemplateParts
	if send.TemplateID != nil {
		parts, err := q.ListTemplatePartsInForceAt(ctx, database.ListTemplatePartsInForceAtParams{
			TemplateID: *send.TemplateID, SentAt: send.CreatedAt,
		})
		if err != nil {
			return 0, false, err
		}
		for _, part := range parts {
			candidates = append(candidates, TemplateParts{Subject: part.Subject, PlainText: part.PlainTextContent, HTML: part.HtmlContent})
		}
	}
	rows, err := q.ListQueueRowsToMask(ctx, send.ID)
	if err != nil {
		return 0, false, err
	}

	masker := NewMasker(send.BodyVariables)
	update := database.SetMaskedQueueContentParams{}
	method := MaskedByVariables
	for _, row := range rows {
		sent := Rendered{Subject: row.Subject, PlainText: row.Body}
		if row.BodyHtml != nil {
			sent.HTML = *row.BodyHtml
		}
		masked, err := masker.Mail(sent, RecipientInfo{FullName: row.RecipientFullName, Email: row.RecipientEmail}, candidates)
		if errors.Is(err, ErrCannotMask) {
			return 0, false, q.ClearSendContent(ctx, send.ID)
		}
		if err != nil {
			return 0, false, err
		}
		if masked.Method == MaskedByPatterns {
			method = MaskedByPatterns
		}
		update.Ids = append(update.Ids, row.ID)
		update.Subjects = append(update.Subjects, masked.Subject)
		update.Bodies = append(update.Bodies, masked.PlainText)
		update.Htmls = append(update.Htmls, masked.HTML)
	}
	if len(update.Ids) > 0 {
		if err := q.SetMaskedQueueContent(ctx, update); err != nil {
			return 0, false, err
		}
	}
	by := string(method)
	if err := q.MarkSendMasked(ctx, database.MarkSendMaskedParams{
		ID:              send.ID,
		BodyVariables:   masker.MaskedVariables(),
		MaskedBy:        &by,
		MaskedVariables: masker.Variables(),
	}); err != nil {
		return 0, false, err
	}
	return len(update.Ids), true, nil
}
