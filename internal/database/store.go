package database

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	*Queries
	Conn *pgxpool.Pool
}

func NewStore(conn *pgxpool.Pool) *Store {
	return &Store{
		Queries: New(conn),
		Conn:    conn,
	}
}

// InTx runs fn in one transaction: it commits when fn returns nil and rolls
// back everything fn wrote when fn returns an error — a check it made after
// writing included.
func (s *Store) InTx(ctx context.Context, fn func(*Queries) error) error {
	return pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		return fn(s.WithTx(tx))
	})
}

// DeleteSentContentViews runs DeleteOldSentContentViews until a batch comes
// back short: it deletes every record of who opened a masked mail older than
// retentionDays, batchSize at a time. It returns how many it deleted.
func (s *Store) DeleteSentContentViews(ctx context.Context, retentionDays, batchSize int) (int64, error) {
	var deleted int64
	for {
		n, err := s.DeleteOldSentContentViews(ctx, DeleteOldSentContentViewsParams{
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

// ClearMailContent runs ClearDueMailContent until a batch comes back short:
// it empties the content of every send due (a sensitive one once none of its
// rows is still to go out, any other past retentionDays), batchSize sends at a
// time so no transaction holds many rows. It returns the sends and the queue
// rows it emptied.
func (s *Store) ClearMailContent(ctx context.Context, retentionDays, batchSize int) (sends int, rows int64, err error) {
	for {
		cleared, err := s.ClearDueMailContent(ctx, ClearDueMailContentParams{
			RetentionDays: retentionDays,
			BatchSize:     batchSize,
		})
		if err != nil {
			return sends, rows, err
		}
		sends += len(cleared)
		for _, send := range cleared {
			rows += send.RowsCleared
		}
		if len(cleared) < batchSize {
			return sends, rows, nil
		}
		if err := ctx.Err(); err != nil {
			return sends, rows, err
		}
	}
}
