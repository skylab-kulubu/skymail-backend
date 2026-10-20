// Package bounce records what a delivery status notification (an NDR) says
// about a mail SkyMail sent, and suppresses an address one permanent failure
// was reported for (hub spec skymail-bounce-isleme §3–§4; README, "Geri dönen
// posta ve bastırma listesi"). The reader that fetches the reports from the
// sender mailbox (ticket 04) hands each one to Process; nothing here reads a
// mailbox.
//
// A report is matched to a queue row only when all of these hold, so a forged
// report cannot suppress an address without knowing a queue row's random id
// and its recipient together:
//
//  1. it is a delivery status notification (dsn.Parse);
//  2. the returned message's Message-ID (or, without one, the report's
//     In-Reply-To, then References) is <queue id@SMTP_FQDN>;
//  3. that row exists, is e-mail, was sent, and the report came after it was
//     (sentAtSlack allowed for the two clocks);
//  4. the report names the row's address (Final-Recipient or
//     Original-Recipient); a report naming another address (a forwarding, an
//     alias) is recorded but never suppresses;
//  5. the report was not recorded before (the SHA-256 of its id).
//
// Nothing logged or stored here holds an address or a report's text.
package bounce

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/dsn"
)

// Outcome is what Process did with a report: the reader counts them.
type Outcome string

const (
	// OutcomeNotDSN: not a delivery status notification (an auto-reply, a
	// person's mail, a plain-text bounce). Nothing recorded.
	OutcomeNotDSN Outcome = "dsn_degil"
	// OutcomeUnmatched: about no mail SkyMail sent and can still name (a
	// foreign Message-ID — the rotator's alarm mail, Keycloak's own SMTP —,
	// no such row, a row not sent, a report from before it was sent, a row
	// Account erasure cleared). Nothing recorded.
	OutcomeUnmatched Outcome = "eslesmedi"
	// OutcomeHard: a permanent failure of the row's address, recorded; the
	// address is suppressed.
	OutcomeHard Outcome = "kalici"
	// OutcomeSoft: another failure, recorded.
	OutcomeSoft Outcome = "yumusak"
	// OutcomeDelay: a delay, recorded.
	OutcomeDelay Outcome = "gecikme"
	// OutcomeOtherRecipient: a failure or delay of another address than the
	// row's (a forwarding, an alias), recorded with recipient_matched false
	// and never suppressed.
	OutcomeOtherRecipient Outcome = "alici_farkli"
	// OutcomeDuplicate: this report was recorded before. Nothing changed.
	OutcomeDuplicate Outcome = "tekrar"
	// OutcomeNoFailure: the report says the mail was delivered, relayed or
	// expanded. Nothing recorded.
	OutcomeNoFailure Outcome = "diger"
)

// RetentionDays is how long a bounce is kept: a year, as open and click
// tracking's events (hub spec §5). The content sweep deletes older ones.
const RetentionDays = 365

// sentAtSlack is how much earlier than the row's sent_at a report may say it
// arrived: Graph's receivedDateTime has whole seconds and Exchange's clock is
// not SkyMail's. Exchange Online's own report of a recipient it has no
// mailbox for comes within a second.
const sentAtSlack = 2 * time.Minute

// NDR is one message from the sender mailbox.
type NDR struct {
	// ID is the message's id in the mailbox (Graph internetMessageId); empty
	// takes the report's own Message-ID.
	ID string
	// ReceivedAt is when the mailbox received it; zero is now.
	ReceivedAt time.Time
	// MIME is the whole message. It is read once and not kept.
	MIME io.Reader
}

// ErrNoID says neither the mailbox nor the report gave the report an id: it
// could not be recorded once.
var ErrNoID = errors.New("the report has no id")

// Process reads one report and records it. fqdn is SMTP_FQDN, the domain of
// SkyMail's Message-IDs. An error is a report that could not be read or a
// database failure: nothing was recorded, and the reader may try it again.
func Process(ctx context.Context, store *database.Store, fqdn string, ndr NDR) (Outcome, error) {
	report, err := dsn.Parse(ndr.MIME)
	if errors.Is(err, dsn.ErrNotDSN) {
		return OutcomeNotDSN, nil
	}
	if err != nil {
		return "", err
	}
	queueID, ok := report.QueueID(fqdn)
	if !ok {
		return OutcomeUnmatched, nil
	}
	id := strings.TrimSpace(ndr.ID)
	if id == "" {
		id = report.MessageID
	}
	if id == "" {
		return "", ErrNoID
	}
	sum := sha256.Sum256([]byte(id))
	idHash := hex.EncodeToString(sum[:])
	received := ndr.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}

	var outcome Outcome
	err = store.InTx(ctx, func(q *database.Queries) error {
		outcome, err = record(ctx, q, queueID, idHash, received, report.Recipients)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("record the report: %w", err)
	}
	return outcome, nil
}

func record(ctx context.Context, q *database.Queries, queueID uuid.UUID, idHash string, received time.Time, recipients []dsn.Recipient) (Outcome, error) {
	row, err := q.LockBounceQueueRow(ctx, queueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutcomeUnmatched, nil
	}
	if err != nil {
		return "", err
	}
	address := strings.ToLower(strings.TrimSpace(row.RecipientEmail))
	if row.Channel != "email" || !row.Status.Valid || row.Status.MailQueueStatus != database.MailQueueStatusSent ||
		row.SentAt == nil || received.Before(row.SentAt.Add(-sentAtSlack)) || address == "" {
		return OutcomeUnmatched, nil
	}

	block, matched, found := pick(recipients, address)
	if !found {
		return OutcomeNoFailure, nil
	}
	kind := dsn.Classify(block)
	bounce := database.InsertMailBounceParams{
		QueueID:          queueID,
		Source:           "dsn",
		Kind:             string(kind),
		Action:           block.Action,
		RecipientMatched: matched,
		NdrIDSha256:      &idHash,
		ReceivedAt:       received,
	}
	if block.Status != "" {
		bounce.Status = &block.Status
	}
	if block.SMTPCode != 0 {
		code := block.SMTPCode
		bounce.SmtpCode = &code
	}
	if block.ASCode != "" {
		bounce.ExoAsCode = &block.ASCode
	}
	n, err := q.InsertMailBounce(ctx, bounce)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return OutcomeDuplicate, nil
	}
	if !matched {
		return OutcomeOtherRecipient, nil
	}
	switch kind {
	case dsn.KindHard:
		if _, err := q.SuppressQueueRowAddress(ctx, database.SuppressQueueRowAddressParams{
			QueueID:    queueID,
			Source:     "dsn",
			ReasonCode: ReasonCode(block),
		}); err != nil {
			return "", err
		}
		return OutcomeHard, nil
	case dsn.KindDelay:
		return OutcomeDelay, nil
	default:
		return OutcomeSoft, nil
	}
}

// pick is the block the report says about address: the first failure or
// delay that names it (matched), else the first failure or delay of any
// address (not matched). found is false when no block reports one.
func pick(recipients []dsn.Recipient, address string) (block dsn.Recipient, matched, found bool) {
	for _, r := range recipients {
		if dsn.Classify(r) == dsn.KindNone {
			continue
		}
		if r.FinalRecipient == address || r.OriginalRecipient == address {
			return r, true, true
		}
		if !found {
			block, found = r, true
		}
	}
	return block, false, found
}

// ReasonCode is what a suppression keeps of a report: its enhanced code, and
// Exchange Online's AS(…) token when there is one (5.4.1 AS(201806281)).
func ReasonCode(r dsn.Recipient) string {
	code := r.Status
	if code == "" && r.SMTPCode != 0 {
		code = fmt.Sprint(r.SMTPCode)
	}
	if r.ASCode != "" {
		code += " AS(" + r.ASCode + ")"
	}
	return strings.TrimSpace(code)
}
