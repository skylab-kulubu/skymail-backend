package mailer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/consent"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// The contact consents applied (KVKK aydınlatma s6 §5.8, §5.10, §5.14, rows
// 13–15; hub tickets kvkk-s6-yayin-oncesi 11 and 12; README, "Tanıtım
// postası ve hesapsız liste alıcısı"). Two kinds of duyuru mail go only to
// people core holds a consent for:
//
//   - a promotional mail (a duyuru send with commercial_content_absent
//     false: its main purpose is promoting a product or a service, such as
//     a discount code, a company's job posting, a sale or a raffle) goes
//     only to an address in core's promotional audience, and carries that
//     grant's withdraw link as its List-Unsubscribe and footer link: one
//     click ends the promotional consent. Always applied, whatever the
//     switches say now, so that a promotional mail queued while they were
//     on never goes to everyone;
//   - with SKYMAIL_LIST_CONSENT on, a mail of a send to an internal list,
//     to an address that is no account's (core's recipients:resolve), goes
//     only to an address in core's event_invitations audience on a text
//     that covers announcements (not davet-v1, core #289 review B6), and
//     not to one due its renewal question. A Keycloak group's members are
//     accounts, and a row that knows its recipient's account is not held.
//
// The check is made where the dispatcher takes the bulk rows, in the same
// transaction and before the send budget counts them, every turn: a consent
// withdrawn after the send was queued holds the mail too. Core is asked at
// most consentGateTimeout a turn. A row held is written failed with nothing
// tried, its error counted skipped (suppressedErrorPrefix), never sent: when
// core does not answer, the audience is none (fail closed). Every duyuru
// mail is queued in the bulk lane, so the system lane is not checked.

const (
	// noPromotionalConsentError: the address gave no promotional consent.
	noPromotionalConsentError = suppressedErrorPrefix + "no_promotional_consent"
	// noAnnouncementConsentError: an address without an account gave no
	// consent to the club's announcements ("duyuru rızası yok").
	noAnnouncementConsentError = suppressedErrorPrefix + "no_announcement_consent"
	// consentUnavailableError: core did not answer, so no consent is known.
	consentUnavailableError = suppressedErrorPrefix + "consent_unavailable"
)

// clubAnnouncements is the category of a send that names none.
const clubAnnouncements = "club:duyuru"

// consentGateTimeout bounds what one turn waits on core.
const consentGateTimeout = 10 * time.Second

// ConsentReader is what the gate asks core (consent.Client).
type ConsentReader interface {
	Audience(ctx context.Context, purpose string) ([]consent.Item, error)
	Matched(ctx context.Context, category string, addresses []string) (map[string]bool, error)
}

// ConsentGate is the gate's configuration (ConsentGateFromEnv).
type ConsentGate struct {
	// Promotional is SKYMAIL_PROMOTIONAL: a duyuru send may say it is
	// promotional (commercial_content_absent false). Off, it is refused.
	Promotional bool
	// ListConsent is SKYMAIL_LIST_CONSENT: a list send's addresses without
	// an account need the announcement consent.
	ListConsent bool
	// Core is nil without CORE_URL: then nothing a consent holds is sent.
	Core ConsentReader
}

const (
	// PromotionalEnv lets a duyuru send be promotional: off (the default,
	// also when unset) or on.
	PromotionalEnv = "SKYMAIL_PROMOTIONAL"
	// ListConsentEnv holds a list send's addresses without an account to
	// the announcement consent: off (the default, also when unset) or on.
	ListConsentEnv = "SKYMAIL_LIST_CONSENT"
)

// ConsentGateFromEnv reads both switches; anything but off or on stops
// startup.
func ConsentGateFromEnv(getenv func(string) string) (ConsentGate, error) {
	var gate ConsentGate
	for name, on := range map[string]*bool{PromotionalEnv: &gate.Promotional, ListConsentEnv: &gate.ListConsent} {
		switch raw := strings.ToLower(strings.TrimSpace(getenv(name))); raw {
		case "", "off":
		case "on":
			*on = true
		default:
			return ConsentGate{}, fmt.Errorf("%s must be off or on, not %q", name, raw)
		}
	}
	return gate, nil
}

// WithConsentGate applies the contact consents (ConsentGateFromEnv). Without
// it both switches are off and no reader is set: a promotional mail is
// never sent.
func WithConsentGate(gate ConsentGate) Option {
	return func(m *mailerImpl) { m.consents = gate }
}

// PromotionalAvailable says SKYMAIL_PROMOTIONAL is on: a duyuru send may be
// promotional.
func (m *mailerImpl) PromotionalAvailable() bool { return m.consents.Promotional }

// gatedRow is a claimed row the gate decides about.
type gatedRow struct {
	id      uuid.UUID
	address string
}

// gateConsents writes failed (in q's transaction) the bulk rows this process
// just took that a contact consent holds back, gives the promotional ones
// their withdraw link, and returns the rows to send.
func (m *mailerImpl) gateConsents(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []database.MailQueue) ([]database.MailQueue, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	found, err := q.GetConsentGateClaimedRows(ctx, database.GetConsentGateClaimedRowsParams{Ids: ids, ClaimedBy: m.claimant})
	if err != nil {
		return nil, fmt.Errorf("read the sends contact consents apply to: %w", err)
	}
	var promotional []gatedRow
	listByCategory := map[string][]gatedRow{}
	for _, row := range found {
		if AccountMail(row.Key, row.System) {
			continue
		}
		address := consent.NormalizeAddress(row.RecipientEmail)
		switch {
		case row.Promotional:
			promotional = append(promotional, gatedRow{row.ID, address})
		case m.consents.ListConsent && row.ToList && row.RecipientSub == nil &&
			LaneOf(m.lanes.purposeOf(row.Purpose), row.Key, row.System, Priority(row.Priority)) == LaneDuyuru:
			category := clubAnnouncements
			if row.Category != nil {
				category = *row.Category
			}
			listByCategory[category] = append(listByCategory[category], gatedRow{row.ID, address})
		}
	}
	if len(promotional) == 0 && len(listByCategory) == 0 {
		return rows, nil
	}

	asking, cancel := context.WithTimeout(ctx, consentGateTimeout)
	defer cancel()
	held := map[uuid.UUID]string{}
	links := map[uuid.UUID]string{}
	if len(promotional) > 0 {
		audience, err := m.promotionalAudience(asking)
		for _, row := range promotional {
			link, ok := audience[row.address]
			switch {
			case err != nil:
				held[row.id] = consentUnavailableError
			case !ok:
				held[row.id] = noPromotionalConsentError
			default:
				links[row.id] = link
			}
		}
		if err != nil {
			logger.Warn().Err(err).Int("count", len(promotional)).
				Msg("Core did not say who consented to promotional mail: those rows are written failed, never sent")
		}
	}
	if len(listByCategory) > 0 {
		m.gateListRows(asking, logger, listByCategory, held)
	}

	if err := m.writeHeld(ctx, q, logger, held); err != nil {
		return nil, err
	}
	if len(links) > 0 {
		linkIDs := make([]uuid.UUID, 0, len(links))
		linkValues := make([]string, 0, len(links))
		for id, link := range links {
			linkIDs = append(linkIDs, id)
			linkValues = append(linkValues, link)
		}
		if _, err := q.SetClaimedRowsUnsubscribeURL(ctx, database.SetClaimedRowsUnsubscribeURLParams{
			Ids: linkIDs, Links: linkValues, ClaimedBy: m.claimant,
		}); err != nil {
			return nil, fmt.Errorf("give promotional mail its withdraw link: %w", err)
		}
	}
	kept := make([]database.MailQueue, 0, len(rows)-len(held))
	for _, row := range rows {
		if _, ok := held[row.ID]; ok {
			continue
		}
		if link, ok := links[row.ID]; ok {
			row.ListUnsubscribeUrl = &link
		}
		kept = append(kept, row)
	}
	return kept, nil
}

// gateListRows holds, in held, the list rows to an address that is no
// account's and gave no announcement consent.
func (m *mailerImpl) gateListRows(ctx context.Context, logger *zerolog.Logger, byCategory map[string][]gatedRow, held map[uuid.UUID]string) {
	var unmatched []gatedRow
	var resolveErr error
	for category, rows := range byCategory {
		matched, err := m.matchedAddresses(ctx, category, rows)
		if err != nil {
			// Who has an account is not known: each row needs the consent.
			resolveErr = err
			unmatched = append(unmatched, rows...)
			continue
		}
		for _, row := range rows {
			if !matched[row.address] {
				unmatched = append(unmatched, row)
			}
		}
	}
	if len(unmatched) == 0 {
		return
	}
	invited, audienceErr := m.announcementAudience(ctx)
	for _, row := range unmatched {
		switch {
		case audienceErr == nil && invited[row.address]:
		case audienceErr != nil || resolveErr != nil:
			held[row.id] = consentUnavailableError
		default:
			held[row.id] = noAnnouncementConsentError
		}
	}
	if err := errors.Join(resolveErr, audienceErr); err != nil {
		logger.Warn().Err(err).Int("count", len(unmatched)).
			Msg("Core did not say which list recipients have an account or consented to announcements: those without it are written failed, never sent")
	}
}

func (m *mailerImpl) matchedAddresses(ctx context.Context, category string, rows []gatedRow) (map[string]bool, error) {
	if m.consents.Core == nil {
		return nil, errors.New("no core to ask (CORE_URL)")
	}
	addresses := make([]string, len(rows))
	for i, row := range rows {
		addresses[i] = row.address
	}
	return m.consents.Core.Matched(ctx, category, addresses)
}

// promotionalAudience is the addresses that consented to promotional mail,
// each with its grant's withdraw link. A grant whose link cannot go into
// List-Unsubscribe (ValidUnsubscribeURL) does not count: the mail would have
// no way out.
func (m *mailerImpl) promotionalAudience(ctx context.Context) (map[string]string, error) {
	if m.consents.Core == nil {
		return nil, errors.New("no core to ask (CORE_URL)")
	}
	items, err := m.consents.Core.Audience(ctx, consent.PurposePromotional)
	if err != nil {
		return nil, err
	}
	audience := make(map[string]string, len(items))
	for _, item := range items {
		if _, ok := audience[item.Email]; !ok && ValidUnsubscribeURL(item.WithdrawURL) {
			audience[item.Email] = item.WithdrawURL
		}
	}
	return audience, nil
}

// announcementAudience is the addresses that consented to the club's other
// events and announcements: a grant on davet-v1 named invitations alone, and
// one due its renewal question gets that question, not announcements.
func (m *mailerImpl) announcementAudience(ctx context.Context) (map[string]bool, error) {
	if m.consents.Core == nil {
		return nil, errors.New("no core to ask (CORE_URL)")
	}
	items, err := m.consents.Core.Audience(ctx, consent.PurposeEventInvitations)
	if err != nil {
		return nil, err
	}
	invited := make(map[string]bool, len(items))
	for _, item := range items {
		if item.TextVersion != consent.TextInvitationsOnly && !item.RenewalDue {
			invited[item.Email] = true
		}
	}
	return invited, nil
}

// writeHeld writes the held rows failed with their reason, nothing tried.
func (m *mailerImpl) writeHeld(ctx context.Context, q *database.Queries, logger *zerolog.Logger, held map[uuid.UUID]string) error {
	byReason := map[string][]uuid.UUID{}
	for id, reason := range held {
		byReason[reason] = append(byReason[reason], id)
	}
	for reason, ids := range byReason {
		if _, err := q.SkipUnsubscribedClaimedRows(ctx, database.SkipUnsubscribedClaimedRowsParams{
			Ids: ids, ClaimedBy: m.claimant, Error: reason,
		}); err != nil {
			return fmt.Errorf("hold rows without a consent: %w", err)
		}
		logger.Info().Int("count", len(ids)).Str("reason", reason).
			Msg("Recipients without the contact consent their mail needs: written failed, never sent")
	}
	return nil
}
