package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// The unsubscribe record (README, "Tek tık çıkış ucu"): who reverses one is
// recorded in mail_unsubscribe_reactivations.
const (
	// UnsubscribeSourceLink is an unsubscribe SkyMail's link wrote: the
	// one-click POST or the page's button.
	UnsubscribeSourceLink = "link"
	// ReactivatedByRecipient is the person, on their link's page; the row
	// has no subject and no reason.
	ReactivatedByRecipient = "recipient"
	// ReactivatedByOperator is the panel, with the operator's subject and
	// the reason they gave.
	ReactivatedByOperator = "operator"
)

// Unsubscribe records address (normalized) out of stream from SkyMail's
// link. It can be repeated: an address already out keeps its first record,
// and created is false.
func (s *Store) Unsubscribe(ctx context.Context, address, stream string) (created bool, err error) {
	n, err := s.InsertUnsubscribe(ctx, InsertUnsubscribeParams{Address: address, Stream: stream, Source: UnsubscribeSourceLink})
	return n > 0, err
}

// Resubscribe removes address's unsubscribe from stream at the person's own
// request (their link's page) and records it, without the address. It can
// be repeated: an address with no record changes nothing, and removed is
// false.
func (s *Store) Resubscribe(ctx context.Context, address, stream string) (removed bool, err error) {
	err = s.InTx(ctx, func(q *Queries) error {
		row, err := q.DeleteUnsubscribeOf(ctx, DeleteUnsubscribeOfParams{Address: address, Stream: stream})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		removed = true
		return q.InsertUnsubscribeReactivation(ctx, InsertUnsubscribeReactivationParams{
			Actor: ReactivatedByRecipient, Stream: row.Stream, Source: row.Source, UnsubscribedAt: row.CreatedAt,
		})
	})
	return removed, err
}
