package mailer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
//     Nor is a mail its sender stated is about the list's own event or work
//     (own_event_statement; s6 row 15: 5/2-c, no consent): it still goes in
//     the duyuru stream, with its way out.
//
// The check is made where the dispatcher takes the bulk rows, in the same
// transaction and before the send budget counts them, every turn: a consent
// withdrawn after the send was queued holds the mail too. Core is never
// asked inside that transaction or under the budget's lock (abonelik-cikisi
// 09): the gate reads what core said from a cache (consentCacheTTL), and
// what it does not know yet is fetched after the transaction, in the
// background (at most consentRoundTimeout), while the dispatcher goes on.
//
//   - Held: a row core's answer says no to is written failed with nothing
//     tried, its error counted skipped (suppressedErrorPrefix), never sent.
//   - Waiting: a row whose answer is not known yet (not fetched, or core did
//     not answer) goes back to pending, nothing tried and nothing counted,
//     and is taken again later: at once while the answer is being fetched,
//     with a backoff by its age while core does not answer (a circuit
//     breaker keeps core unasked for consentBreakerOpen after a failure).
//     Not knowing a consent is not its absence: a list's member is never
//     held because core was down. A row that core, failing (its breaker
//     open), still has not answered for consentUndecidedAfter after it was
//     queued is written failed, consent: unavailable (no suppressed: prefix:
//     it was never decided).
//
// Every duyuru mail is queued in the bulk lane, so the system lane is not
// checked; its rows are handed to the workers before the bulk lane is taken.

const (
	// noPromotionalConsentError: the address gave no promotional consent.
	noPromotionalConsentError = suppressedErrorPrefix + "no_promotional_consent"
	// noAnnouncementConsentError: an address without an account gave no
	// consent to the club's announcements ("duyuru rızası yok").
	noAnnouncementConsentError = suppressedErrorPrefix + "no_announcement_consent"
	// consentUndecidedError: core did not answer for consentUndecidedAfter,
	// so no consent is known; not skipped, undecided.
	consentUndecidedError = "consent: unavailable"
)

// clubAnnouncements is the category of a send that names none.
const clubAnnouncements = "club:duyuru"

const (
	// consentRoundTimeout bounds one fetch from core (abonelik-cikisi 09: a
	// turn waits at most 5 seconds).
	consentRoundTimeout = 5 * time.Second
	// consentCacheTTL is how long an answer is used: a consent withdrawn
	// counts at most this late, well within Google's and Yahoo's two days.
	consentCacheTTL = 60 * time.Second
	// consentBreakerOpen is how long core is not asked after it failed.
	consentBreakerOpen = 30 * time.Second
	// consentUndecidedAfter is how long a row waits for core's answer.
	consentUndecidedAfter = 48 * time.Hour
	// consentFetchDelay is how soon a row waiting for a fetch under way is
	// taken again.
	consentFetchDelay = time.Second
)

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

// CheckWith says whether the gate can run with lanes: a promotional mail
// must carry its withdraw link both ways s6 §5.8 and RFC 8058 ask for, so
// SKYMAIL_PROMOTIONAL=on needs SKYMAIL_MAIL_PURPOSE=on (a send names its
// purpose, and the visible footer line is added), SKYMAIL_LIST_UNSUBSCRIBE=on
// and a duyuru lane that is not off (List-Unsubscribe is written only
// there). Anything less stops startup: SkyMail never says promotional sends
// are on while their mail would go out without its one-click way out.
//
// SKYMAIL_LIST_CONSENT=on needs SKYMAIL_MAIL_PURPOSE=on too: without
// purposes no send can state that it is about the list's own event, so the
// gate would hold every such mail to a guest.
func (g ConsentGate) CheckWith(lanes SendLanes) error {
	if g.ListConsent && !lanes.Purpose {
		return fmt.Errorf("%s=on needs %s=on", ListConsentEnv, PurposeEnv)
	}
	if !g.Promotional {
		return nil
	}
	switch {
	case !lanes.Purpose:
		return fmt.Errorf("%s=on needs %s=on", PromotionalEnv, PurposeEnv)
	case !lanes.ListUnsubscribe:
		return fmt.Errorf("%s=on needs %s=on", PromotionalEnv, ListUnsubscribeEnv)
	case lanes.Lane(LaneDuyuru).Transport == TransportOff:
		return fmt.Errorf("%s=on needs %s=graph or xoauth2: List-Unsubscribe is written only on the duyuru lane", PromotionalEnv, sendEnv(LaneDuyuru))
	}
	return nil
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

// ListConsentAvailable says SKYMAIL_LIST_CONSENT is on.
func (m *mailerImpl) ListConsentAvailable() bool { return m.consents.ListConsent }

// consentCache is what core said, shared by the dispatcher and the fetch
// that runs after its transaction.
type consentCache struct {
	mu sync.Mutex
	// promotional: address → withdraw link; invited: the addresses whose
	// announcement consent counts; accounts: address → whether core matched
	// it to an account. Each with when it was read.
	promotional   map[string]string
	promotionalAt time.Time
	invited       map[string]bool
	invitedAt     time.Time
	accounts      map[string]cachedAccount
	// openUntil: core failed, and is not asked again before then.
	openUntil time.Time
	// needs is what the last turns found unknown, for the next fetch.
	needs consentNeeds
	// fetching: a fetch is under way.
	fetching bool

	ttl, roundTimeout, breakerOpen, fetchDelay time.Duration
	now                                        func() time.Time
}

type cachedAccount struct {
	matched bool
	at      time.Time
}

// consentNeeds is what a fetch reads.
type consentNeeds struct {
	promotional, invited bool
	addresses            map[string]bool
}

func (n consentNeeds) empty() bool { return !n.promotional && !n.invited && len(n.addresses) == 0 }

func newConsentCache() *consentCache {
	return &consentCache{accounts: map[string]cachedAccount{}, ttl: consentCacheTTL, roundTimeout: consentRoundTimeout,
		breakerOpen: consentBreakerOpen, fetchDelay: consentFetchDelay, now: time.Now}
}

func (c *consentCache) fresh(at time.Time) bool { return !at.IsZero() && c.now().Sub(at) < c.ttl }

// gatedRow is a claimed row the gate decides about.
type gatedRow struct {
	id      uuid.UUID
	address string
	queued  time.Time
}

// verdict is what the gate decides about a row.
type verdict struct {
	held    map[uuid.UUID]string
	links   map[uuid.UUID]string
	waiting []gatedRow
}

// gateConsents decides (in q's transaction, from the cache alone) about the
// bulk rows this process just took that a contact consent applies to: it
// writes the held failed, puts the waiting back, gives the promotional ones
// their withdraw link and returns the rows to send.
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
	var promotional, list []gatedRow
	for _, row := range found {
		gated := gatedRow{id: row.ID, address: consent.NormalizeAddress(row.RecipientEmail), queued: m.consentCache.now()}
		if row.CreatedAt != nil {
			gated.queued = *row.CreatedAt
		}
		switch {
		// Before account mail: a promotional mail is held whatever its
		// template, a System template's too.
		case row.Promotional:
			promotional = append(promotional, gated)
		case AccountMail(row.Key, row.System):
		// A mail stated to be about the list's own event or work goes
		// without the consent (s6 row 15, 5/2-c), in the duyuru stream still.
		case m.consents.ListConsent && row.ToList && row.RecipientSub == nil && !row.OwnEvent &&
			LaneOf(m.lanes.purposeOf(row.Purpose), row.Key, row.System, Priority(row.Priority)) == LaneDuyuru:
			list = append(list, gated)
		}
	}
	if len(promotional) == 0 && len(list) == 0 {
		return rows, nil
	}

	v := m.consentCache.decide(promotional, list)
	if err := m.writeHeld(ctx, q, logger, v.held); err != nil {
		return nil, err
	}
	if err := m.writeWaiting(ctx, q, logger, v.waiting); err != nil {
		return nil, err
	}
	if len(v.links) > 0 {
		linkIDs := make([]uuid.UUID, 0, len(v.links))
		linkValues := make([]string, 0, len(v.links))
		for id, link := range v.links {
			linkIDs = append(linkIDs, id)
			linkValues = append(linkValues, link)
		}
		if _, err := q.SetClaimedRowsUnsubscribeURL(ctx, database.SetClaimedRowsUnsubscribeURLParams{
			Ids: linkIDs, Links: linkValues, ClaimedBy: m.claimant,
		}); err != nil {
			return nil, fmt.Errorf("give promotional mail its withdraw link: %w", err)
		}
	}
	waiting := make(map[uuid.UUID]bool, len(v.waiting))
	for _, row := range v.waiting {
		waiting[row.id] = true
	}
	kept := make([]database.MailQueue, 0, len(rows))
	for _, row := range rows {
		if _, ok := v.held[row.ID]; ok || waiting[row.ID] {
			continue
		}
		if link, ok := v.links[row.ID]; ok {
			row.ListUnsubscribeUrl = &link
		}
		kept = append(kept, row)
	}
	return kept, nil
}

// decide sorts the rows by what the cache knows, and notes what it does not.
func (c *consentCache) decide(promotional, list []gatedRow) verdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := verdict{held: map[uuid.UUID]string{}, links: map[uuid.UUID]string{}}
	for _, row := range promotional {
		if !c.fresh(c.promotionalAt) {
			c.needs.promotional = true
			v.waiting = append(v.waiting, row)
			continue
		}
		if link, ok := c.promotional[row.address]; ok {
			v.links[row.id] = link
		} else {
			v.held[row.id] = noPromotionalConsentError
		}
	}
	invitedKnown := c.fresh(c.invitedAt)
	for _, row := range list {
		account, accountKnown := c.accounts[row.address]
		accountKnown = accountKnown && c.fresh(account.at)
		switch {
		case accountKnown && account.matched:
		case invitedKnown && c.invited[row.address]:
		case accountKnown && invitedKnown:
			v.held[row.id] = noAnnouncementConsentError
		default:
			if !accountKnown {
				if c.needs.addresses == nil {
					c.needs.addresses = map[string]bool{}
				}
				c.needs.addresses[row.address] = true
			}
			if !invitedKnown {
				c.needs.invited = true
			}
			v.waiting = append(v.waiting, row)
		}
	}
	return v
}

// writeWaiting puts the waiting rows back to pending, nothing tried: at once
// while core may be asked, with a backoff by their age while its breaker is
// open; a row waiting past consentUndecidedAfter is written failed,
// undecided.
func (m *mailerImpl) writeWaiting(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []gatedRow) error {
	c := m.consentCache
	c.mu.Lock()
	now, open, fetchDelay := c.now(), c.now().Before(c.openUntil), c.fetchDelay
	c.mu.Unlock()
	return m.deferWaiting(ctx, q, logger, rows, waitingRule{
		now: now, open: open, fetchDelay: fetchDelay, undecidedAfter: consentUndecidedAfter, undecidedError: consentUndecidedError,
		undecidedMessage: "Core never said whether recipients consented: written failed, undecided, never sent",
		waitingMessage:   "Recipients wait for core's consent answer: put back, nothing tried",
	})
}

// waitingRule is how rows waiting for core are put back at now: open says
// core's breaker is open, fetchDelay how soon a row is taken again while core may
// be asked, and a row waiting undecidedAfter while core fails is written
// failed with undecidedError.
type waitingRule struct {
	now                              time.Time
	open                             bool
	fetchDelay, undecidedAfter       time.Duration
	undecidedError                   string
	undecidedMessage, waitingMessage string
}

// deferWaiting puts rows waiting for an answer of core back to pending,
// nothing tried, as rule says (the contact consents', consent_gate.go, and
// the bulk preference check's, preference_gate.go).
func (m *mailerImpl) deferWaiting(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []gatedRow, rule waitingRule) error {
	if len(rows) == 0 {
		return nil
	}
	now := rule.now
	byDelay := map[time.Duration][]uuid.UUID{}
	var undecided []uuid.UUID
	for _, row := range rows {
		age := now.Sub(row.queued)
		switch {
		// Only while core really fails: a cold or stale cache is no
		// failure, and an old row (a full day's budget, a paused sender)
		// is asked about like any other.
		case rule.open && age >= rule.undecidedAfter:
			undecided = append(undecided, row.id)
		case rule.open:
			byDelay[consentBackoff(age)] = append(byDelay[consentBackoff(age)], row.id)
		default:
			byDelay[rule.fetchDelay] = append(byDelay[rule.fetchDelay], row.id)
		}
	}
	for delay, ids := range byDelay {
		if _, err := q.DeferClaimedRows(ctx, database.DeferClaimedRowsParams{
			Ids: ids, ClaimedBy: m.claimant, DelayMs: int(delay / time.Millisecond),
		}); err != nil {
			return fmt.Errorf("put back rows waiting for core's answer: %w", err)
		}
	}
	if len(undecided) > 0 {
		if _, err := q.SkipUnsubscribedClaimedRows(ctx, database.SkipUnsubscribedClaimedRowsParams{
			Ids: undecided, ClaimedBy: m.claimant, Error: rule.undecidedError,
		}); err != nil {
			return fmt.Errorf("close rows core never answered for: %w", err)
		}
		logger.Warn().Int("count", len(undecided)).Dur("waited", rule.undecidedAfter).Msg(rule.undecidedMessage)
	}
	if waiting := len(rows) - len(undecided); waiting > 0 {
		logger.Info().Int("count", waiting).Bool("core_breaker_open", rule.open).Msg(rule.waitingMessage)
	}
	return nil
}

// consentBackoff is how long a row of age waits while core does not answer:
// 1, 2, 5, 10, then 15 minutes, as it grows older.
func consentBackoff(age time.Duration) time.Duration {
	switch {
	case age < 2*time.Minute:
		return time.Minute
	case age < 5*time.Minute:
		return 2 * time.Minute
	case age < 15*time.Minute:
		return 5 * time.Minute
	case age < 30*time.Minute:
		return 10 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// fetchConsents starts, unless one is under way or core's breaker is open,
// a fetch of what the last turns found unknown, outside any transaction and
// the dispatcher's turn; it wakes the dispatcher when it is done.
func (m *mailerImpl) fetchConsents(ctx context.Context, logger *zerolog.Logger) {
	c := m.consentCache
	c.mu.Lock()
	if c.fetching || c.needs.empty() || c.now().Before(c.openUntil) {
		c.mu.Unlock()
		return
	}
	needs := c.needs
	c.needs = consentNeeds{}
	c.fetching = true
	c.mu.Unlock()
	m.consentFetches.Add(1)
	go func() {
		defer m.consentFetches.Done()
		err := m.fetch(ctx, needs)
		c.mu.Lock()
		c.fetching = false
		if err != nil {
			c.openUntil = c.now().Add(c.breakerOpen)
		}
		c.mu.Unlock()
		if err != nil {
			logger.Warn().Err(err).Dur("breaker_open", c.breakerOpen).
				Msg("Core did not say who consented: the rows that need it wait")
		}
		m.wake()
	}()
}

// fetch reads what needs names from core, within consentRoundTimeout, and
// keeps it in the cache.
func (m *mailerImpl) fetch(ctx context.Context, needs consentNeeds) error {
	if m.consents.Core == nil {
		return errors.New("no core to ask (CORE_URL)")
	}
	c := m.consentCache
	asking, cancel := context.WithTimeout(ctx, c.roundTimeout)
	defer cancel()
	if needs.promotional {
		audience, err := m.promotionalAudience(asking)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.promotional, c.promotionalAt = audience, c.now()
		c.mu.Unlock()
	}
	if needs.invited {
		invited, err := m.announcementAudience(asking)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.invited, c.invitedAt = invited, c.now()
		c.mu.Unlock()
	}
	if len(needs.addresses) > 0 {
		addresses := make([]string, 0, len(needs.addresses))
		for address := range needs.addresses {
			addresses = append(addresses, address)
		}
		// Matching an address does not depend on the category: club:duyuru
		// is always one core knows.
		matched, err := m.consents.Core.Matched(asking, clubAnnouncements, addresses)
		if err != nil {
			return err
		}
		c.mu.Lock()
		at := c.now()
		for address, ok := range matched {
			c.accounts[address] = cachedAccount{matched: ok, at: at}
		}
		for address, account := range c.accounts {
			if !c.fresh(account.at) {
				delete(c.accounts, address)
			}
		}
		c.mu.Unlock()
	}
	return nil
}

// promotionalAudience is the addresses that consented to promotional mail,
// each with its grant's withdraw link. A grant whose link cannot go into
// List-Unsubscribe (ValidUnsubscribeURL) does not count: the mail would have
// no way out.
func (m *mailerImpl) promotionalAudience(ctx context.Context) (map[string]string, error) {
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
// events and announcements: a grant on davet-v1 named invitations alone, one
// that names no text is not known to cover them, and one due its renewal
// question gets that question, not announcements.
func (m *mailerImpl) announcementAudience(ctx context.Context) (map[string]bool, error) {
	items, err := m.consents.Core.Audience(ctx, consent.PurposeEventInvitations)
	if err != nil {
		return nil, err
	}
	invited := make(map[string]bool, len(items))
	for _, item := range items {
		if item.TextVersion != "" && item.TextVersion != consent.TextInvitationsOnly && !item.RenewalDue {
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
