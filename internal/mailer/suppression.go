package mailer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The suppression list applied (hub spec skymail-bounce-isleme §4, ticket
// 05; README, "Geri dönen posta ve bastırma listesi"). An address a relay
// refused for good (smtpReply.permanent) or a delivery status notification
// reported as not existing (internal/bounce) is in mail_suppressions. When a
// send is queued, a recipient at such an address is written failed with
// suppressedError and never reaches the relay or the send budget, as a
// reserved domain's — unless the send is account mail, which is sent and
// flagged.

// suppressedErrorPrefix starts the error a suppressed recipient is written
// failed with: "suppressed: dsn 5.1.10". mail_task_status() counts such a row
// skipped, not failed (20261021090100), by this prefix.
const suppressedErrorPrefix = "suppressed: "

// suppressedError is the row's error: the suppression's source and its codes,
// never the address (mail_suppressions.reason_code holds codes only).
func suppressedError(s database.GetSuppressedAddressesRow) string {
	return suppressedErrorPrefix + s.Source + " " + s.ReasonCode
}

// AccountMail says a send of the template with this key is account mail: a
// login or sign-up code, an address verification, a password reset — what a
// person needs to reach their account (keycloak.*, account.*). It goes to a
// suppressed address anyway, flagged (Yusuf, 2026-10-20: hub spec §0 K3).
// Everything else — Forms, Mail onayı, lists — is suppressed.
// The key alone is not enough: the template must also be a system template
// (the Template seed's; the keys are not reserved, so anyone with
// skymail:templates:write could otherwise send past the list with an
// account.* key). Only skymail:templates:system, the seed's service account's,
// marks one (handlers.TemplatesSystemRole).
func AccountMail(key *string, system bool) bool {
	return system && key != nil && (strings.HasPrefix(*key, "keycloak.") || strings.HasPrefix(*key, "account."))
}

// suppressionAddress is how an address is looked up in mail_suppressions.
func suppressionAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// suppressionsOf reads the suppressed among rows' recipients, and whether
// their send is account mail (asked only when one is suppressed).
func (q *queue) suppressionsOf(ctx context.Context, rows []commonMailRow) (map[string]database.GetSuppressedAddressesRow, bool, error) {
	addresses := make([]string, 0, len(rows))
	for _, row := range rows {
		addresses = append(addresses, suppressionAddress(row.RecipientEmail))
	}
	found, err := q.q.GetSuppressedAddresses(ctx, addresses)
	if err != nil {
		return nil, false, fmt.Errorf("read the suppression list: %w", err)
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	suppressed := make(map[string]database.GetSuppressedAddressesRow, len(found))
	for _, s := range found {
		suppressed[s.Address] = s
	}
	template, err := q.q.GetMailTaskTemplateKey(ctx, rows[0].TaskID)
	if err != nil {
		return nil, false, fmt.Errorf("read the send's template: %w", err)
	}
	return suppressed, AccountMail(template.Key, template.System), nil
}

// suppressRefused suppresses the address a relay refused for good, and
// records the refusal as a bounce, once the row is written failed. A failure
// here is logged: the row's outcome is already recorded.
func (m *mailerImpl) suppressRefused(ctx context.Context, logger *zerolog.Logger, job database.MailQueue, sendErr error) {
	var reply smtpReply
	if job.Channel != ChannelEmail || !errors.As(sendErr, &reply) || !reply.permanent() {
		return
	}
	reason := fmt.Sprintf("%s %d", reply.stage, reply.code)
	var status *string
	if reply.enhanced != "" {
		reason += " " + reply.enhanced
		status = &reply.enhanced
	}
	err := m.db.InTx(ctx, func(q *database.Queries) error {
		if err := q.InsertSMTPRefusalBounce(ctx, database.InsertSMTPRefusalBounceParams{
			QueueID: job.ID, Status: status, SmtpCode: reply.code,
		}); err != nil {
			return err
		}
		_, err := q.SuppressQueueRowAddress(ctx, database.SuppressQueueRowAddressParams{
			QueueID: job.ID, Source: "smtp", ReasonCode: reason,
		})
		return err
	})
	if err != nil {
		logger.Err(err).Str("job_id", job.ID.String()).Msg("Failed to suppress the address the relay refused")
		return
	}
	logger.Info().Str("job_id", job.ID.String()).Str("reason", reason).
		Msg("The relay refused the address for good: suppressed")
}
