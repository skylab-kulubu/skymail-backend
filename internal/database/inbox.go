package database

import (
	"context"
	"time"
)

// DeleteExpiredInbox runs DeleteExpiredInboxItems until a batch comes back
// short: it deletes every inbox item whose expiry is at or before at,
// batchSize at a time. It returns how many it deleted.
func (s *Store) DeleteExpiredInbox(ctx context.Context, at time.Time, batchSize int) (int64, error) {
	var deleted int64
	for {
		n, err := s.DeleteExpiredInboxItems(ctx, DeleteExpiredInboxItemsParams{At: at, BatchSize: batchSize})
		deleted += n
		if err != nil || n < int64(batchSize) {
			return deleted, err
		}
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
	}
}
