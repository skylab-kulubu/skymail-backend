package database

import "context"

// DeleteRecipientHistoryViews runs DeleteOldRecipientHistoryViews until a
// batch comes back short: it deletes every record of who read a person's
// mail history older than retentionDays, batchSize at a time. It returns how
// many it deleted.
func (s *Store) DeleteRecipientHistoryViews(ctx context.Context, retentionDays, batchSize int) (int64, error) {
	var deleted int64
	for {
		n, err := s.DeleteOldRecipientHistoryViews(ctx, DeleteOldRecipientHistoryViewsParams{
			RetentionDays: retentionDays,
			BatchSize:     batchSize,
		})
		deleted += n
		if err != nil || n < int64(batchSize) {
			return deleted, err
		}
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
	}
}
