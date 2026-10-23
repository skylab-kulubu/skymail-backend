package mailer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// The unsubscribe record applied (ADR 0072 decision 5; hub ticket
// abonelik-cikisi 02; README, "Tek tık çıkış ucu"). An address that left the
// duyuru stream through SkyMail's link is in mail_unsubscribes. Only mail of
// that stream stops: a send whose sender lane is duyuru (LaneOf: its purpose
// while SKYMAIL_MAIL_PURPOSE is on, else its queue priority). Account mail and the bilgi lane's (Forms
// confirmations, notices) are sent as before, and the bounce list applies to
// every stream as ever: an address in both is held by both.
//
// The record is read twice: when a send is queued, where such a recipient
// is written failed with unsubscribedError and never reaches the relay or
// the send budget; and when the dispatcher takes the duyuru rows, so an
// address that left after its mail was queued gets none of it either. It is
// applied whatever SKYMAIL_UNSUBSCRIBE_LINK says: turning the link off never
// mails an address that left.

// unsubscribedError is the error of a recipient skipped for the record.
// mail_task_status() counts it skipped, not failed, by suppressedErrorPrefix
// (20261021090100): it never makes a send read failed.
const unsubscribedError = suppressedErrorPrefix + "unsubscribed"

// unsubscribesOf reads the unsubscribed among rows' recipients when their
// send is in the duyuru stream: nil for any other send. An islem send is
// never looked up, nor one without a purpose in the system lane (a login
// code waits for no lookup); a duyuru send always is, whatever its lane.
func (q *queue) unsubscribesOf(ctx context.Context, rows []commonMailRow) (map[string]bool, error) {
	purpose := q.lanes.purposeOf(rows[0].Purpose)
	switch {
	case purpose != nil && *purpose == PurposeIslem:
		return nil, nil
	case purpose == nil && q.priority > PriorityBulk:
		return nil, nil
	}
	addresses := make([]string, 0, len(rows))
	for _, row := range rows {
		addresses = append(addresses, suppressionAddress(row.RecipientEmail))
	}
	found, err := q.q.GetUnsubscribedAddresses(ctx, database.GetUnsubscribedAddressesParams{
		Stream: unsubscribe.StreamDuyuru, Addresses: addresses,
	})
	if err != nil {
		return nil, fmt.Errorf("read the unsubscribe record: %w", err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	template, err := q.q.GetMailTaskTemplateKey(ctx, rows[0].TaskID)
	if err != nil {
		return nil, fmt.Errorf("read the send's template: %w", err)
	}
	if LaneOf(purpose, template.Key, template.System, q.priority) != LaneDuyuru {
		return nil, nil
	}
	unsubscribed := make(map[string]bool, len(found))
	for _, address := range found {
		unsubscribed[address] = true
	}
	return unsubscribed, nil
}

// skipUnsubscribed writes failed, with unsubscribedError and nothing tried,
// the bulk rows this process just took (in q's transaction) whose address
// left the duyuru stream since they were queued, and returns the others:
// only those are sent and counted in the send budget.
func (m *mailerImpl) skipUnsubscribed(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []database.MailQueue) ([]database.MailQueue, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	priority := map[uuid.UUID]Priority{}
	for _, row := range rows {
		ids = append(ids, row.ID)
		priority[row.ID] = Priority(row.Priority)
	}
	found, err := q.GetUnsubscribedClaimedRows(ctx, database.GetUnsubscribedClaimedRowsParams{Ids: ids, ClaimedBy: m.claimant})
	if err != nil {
		return nil, fmt.Errorf("read the unsubscribe record: %w", err)
	}
	skip := map[uuid.UUID]bool{}
	var skipIDs []uuid.UUID
	for _, row := range found {
		if LaneOf(m.lanes.purposeOf(row.Purpose), row.Key, row.System, priority[row.ID]) == LaneDuyuru {
			skip[row.ID] = true
			skipIDs = append(skipIDs, row.ID)
		}
	}
	if len(skipIDs) == 0 {
		return rows, nil
	}
	if _, err := q.SkipUnsubscribedClaimedRows(ctx, database.SkipUnsubscribedClaimedRowsParams{
		Ids: skipIDs, ClaimedBy: m.claimant, Error: unsubscribedError,
	}); err != nil {
		return nil, fmt.Errorf("skip the unsubscribed: %w", err)
	}
	logger.Warn().Int("count", len(skipIDs)).
		Msg("Recipients left the duyuru stream after their mail was queued: written failed, never sent")
	kept := make([]database.MailQueue, 0, len(rows)-len(skipIDs))
	for _, row := range rows {
		if !skip[row.ID] {
			kept = append(kept, row)
		}
	}
	return kept, nil
}
