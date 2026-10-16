package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// RecordTracking serializes a recipient's writes across replicas. Erasure
// also updates that recipient, so a committed withdrawal cannot be followed
// by a writer which had observed the old enabled flag.
func (s *Store) RecordTracking(ctx context.Context, token []byte, linkID int64, ua, method string) (bool, error) {
	recorded := false
	err := s.InTx(ctx, func(q *Queries) error {
		recipient, err := q.LockTrackingRecipient(ctx, token)
		if err != nil {
			return err
		}
		opened, err := q.TrackingHasOpen(ctx, recipient.ID)
		if err != nil {
			return err
		}
		kind := "open"
		if linkID != 0 {
			kind = "click"
		}
		classification := tracking.Classify(ua, method, kind, *recipient.SentAt, recipient.At, opened)
		count, err := q.InsertTrackingEvent(ctx, InsertTrackingEventParams{
			QueueID: recipient.ID, Kind: kind, LinkID: pgtype.Int8{Int64: linkID, Valid: linkID != 0}, At: recipient.At,
			SourceClass: classification.SourceClass, AutomatedLikely: classification.AutomatedLikely, ClassifierVersion: classification.Version,
		})
		if err != nil {
			return err
		}
		recorded = count == 1
		if recorded && linkID != 0 {
			return q.MarkTrackingScannerBurst(ctx, MarkTrackingScannerBurstParams{QueueID: recipient.ID, At: recipient.At, ClassifierVersion: classification.Version})
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgx.ErrNoRows
	}
	return recorded, err
}
