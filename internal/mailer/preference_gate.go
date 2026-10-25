package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/consent"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// The bulk preference check (ADR 0072 decisions 3 and 6; hub ticket
// abonelik-cikisi 09; README, "Toplu postada tercih"). While
// SKYMAIL_BULK_PREFERENCE is on, every send of the duyuru stream obeys the
// person's preference in core at the moment it is sent, and every mail of it
// carries a one-click way out:
//
//   - When a send is queued (before its transaction begins: core is never
//     waited for holding one; a Mail onayı request at its approval), core is
//     asked about its recipients (recipients:resolve, by sub when the send
//     knows it, else by address). A recipient core allows gets the person's
//     link and the time it was checked; one core says no to is written
//     failed, nothing tried, "preference: <reason>" (opted_out, inactive,
//     ambiguous), and counted skipped; an address core matches to no account
//     gets SkyMail's own link (unsubscribe.Config.Link). Core not answering
//     writes nothing: the request is 503 mail_task.preference_unavailable
//     (fail closed, ADR 0072); a team category core does not know is 400
//     mail_task.category_unknown.
//   - Where the dispatcher takes the bulk rows, in the same transaction and
//     before the send budget counts them, a row core gave a verdict about is
//     decided again (or, a row that knows its person, decided at last: a
//     news send's), every turn, from what core said at most
//     preferenceCacheTTL ago; what is not known is fetched after the
//     transaction, in the background, in one request per category, and the
//     row waits (as the contact consents' rows do, consent_gate.go). A row
//     core still has not answered about after preferenceUndecidedAfter while
//     core fails is written failed, "preference: unavailable" (undecided,
//     not skipped). An address without an account waits for nothing: its
//     unsubscribe record is read there anyway (unsubscribe.go).
//   - The safety net: a duyuru row without a link is never sent; it is
//     written failed, unsubscribe_missing.
//
// Off, nothing of this runs: every send goes as before.

// PreferenceEnv switches the bulk preference check on: off (the default,
// also when unset) or on.
const PreferenceEnv = "SKYMAIL_BULK_PREFERENCE"

const (
	// preferencePageTimeout bounds one recipients:resolve call when a send is
	// queued, preferenceSendTimeout all of them for one send.
	preferencePageTimeout = 10 * time.Second
	preferenceSendTimeout = 30 * time.Second
	// preferenceRoundTimeout bounds one fetch after a dispatcher turn.
	preferenceRoundTimeout = 5 * time.Second
	// preferenceCacheTTL is how old a verdict the dispatcher still uses: a
	// person who turns a category off after their mail was queued gets none
	// of it from this late on, far within Google's and Yahoo's two days.
	preferenceCacheTTL = 60 * time.Second
	// preferenceBreakerOpen is how long core is not asked after it failed.
	preferenceBreakerOpen = 30 * time.Second
	// preferenceUndecidedAfter is how long a row waits for core's answer.
	preferenceUndecidedAfter = 48 * time.Hour
	// preferenceFetchDelay is how soon a row waiting for a fetch under way is
	// taken again.
	preferenceFetchDelay = time.Second
	// PreferenceRetryAfter is the Retry-After of a send refused because core
	// did not answer: core's own directory answers 503 with 30 seconds.
	PreferenceRetryAfter = 30
)

const (
	// preferenceErrorPrefix and core's reason: a recipient core says may not
	// get the mail. mail_task_status() counts the three reasons skipped
	// (20261025200000).
	preferenceErrorPrefix = "preference: "
	// preferenceUndecidedError: core did not answer for
	// preferenceUndecidedAfter; never decided, so not skipped.
	preferenceUndecidedError = preferenceErrorPrefix + "unavailable"
	// preferenceCategoryGoneError: the send's team category is no team any
	// more (deleted since the send was queued); nobody can say.
	preferenceCategoryGoneError = preferenceErrorPrefix + "category_unknown"
	// unsubscribeMissingError: a duyuru row without a link while the check is
	// on (the safety net).
	unsubscribeMissingError = "unsubscribe_missing"
)

var (
	// ErrPreferenceUnavailable refuses a duyuru send core said nothing about
	// in time: nothing is written, and it may be sent again in a moment.
	ErrPreferenceUnavailable = apperrors.New("mail_task.preference_unavailable",
		"core did not say whom of the recipients the mail may go to (communication preferences): nothing was queued. Try again in a moment.",
		http.StatusServiceUnavailable).WithRetryAfter(PreferenceRetryAfter)
	// ErrPreferenceCategoryUnknown refuses a duyuru send whose team category
	// core calls no team's own.
	ErrPreferenceCategoryUnknown = apperrors.New("mail_task.category_unknown",
		"The category's group is not a team: choose club-wide or a team.", http.StatusBadRequest)
)

// PreferenceResolver is what the check asks core (consent.Client).
type PreferenceResolver interface {
	Resolve(ctx context.Context, category string, recipients []consent.Recipient) ([]consent.Result, error)
}

// PreferenceGate is the check's configuration (PreferenceGateFromEnv).
type PreferenceGate struct {
	// On is SKYMAIL_BULK_PREFERENCE.
	On bool
	// Core answers recipients:resolve; nil without CORE_URL, and then every
	// duyuru send is refused 503.
	Core PreferenceResolver
	// Links is SkyMail's own unsubscribe link: an address core matches to no
	// account gets it.
	Links unsubscribe.Config
}

// PreferenceGateFromEnv reads SKYMAIL_BULK_PREFERENCE; anything but off or on
// stops startup.
func PreferenceGateFromEnv(getenv func(string) string) (PreferenceGate, error) {
	switch raw := strings.ToLower(strings.TrimSpace(getenv(PreferenceEnv))); raw {
	case "", "off":
		return PreferenceGate{}, nil
	case "on":
		return PreferenceGate{On: true}, nil
	default:
		return PreferenceGate{}, fmt.Errorf("%s must be off or on, not %q", PreferenceEnv, raw)
	}
}

// CheckWith says whether the check can run with lanes and links: every
// duyuru mail must leave with its way out, both in its body and in
// List-Unsubscribe. So SKYMAIL_BULK_PREFERENCE=on needs SKYMAIL_MAIL_PURPOSE=on
// (a send names its category, the footer line is added),
// SKYMAIL_LIST_UNSUBSCRIBE=on and a duyuru lane that is not off (the header is
// written only there), and SkyMail's own link on with a key (an address
// without an account gets it). Anything less stops startup.
func (g PreferenceGate) CheckWith(lanes SendLanes) error {
	if !g.On {
		return nil
	}
	switch {
	case !lanes.Purpose:
		return fmt.Errorf("%s=on needs %s=on", PreferenceEnv, PurposeEnv)
	case !lanes.ListUnsubscribe:
		return fmt.Errorf("%s=on needs %s=on", PreferenceEnv, ListUnsubscribeEnv)
	case lanes.Lane(LaneDuyuru).Transport == TransportOff:
		return fmt.Errorf("%s=on needs %s=graph or xoauth2: List-Unsubscribe is written only on the duyuru lane", PreferenceEnv, sendEnv(LaneDuyuru))
	case !g.Links.On || g.Links.BaseURL == "":
		return fmt.Errorf("%s=on needs %s=on: an address without an account gets SkyMail's own link", PreferenceEnv, unsubscribe.LinkEnv)
	case g.Links.Ring.Empty():
		return fmt.Errorf("%s=on needs a key in %s<n> and %s", PreferenceEnv, unsubscribe.KeyEnvPrefix, unsubscribe.CurrentKeyEnv)
	}
	return nil
}

// WithPreferenceGate applies the bulk preference check
// (PreferenceGateFromEnv). Without it the check is off.
func WithPreferenceGate(gate PreferenceGate) Option {
	return func(m *mailerImpl) { m.preferences = gate }
}

// PreferenceAvailable says SKYMAIL_BULK_PREFERENCE is on.
func (m *mailerImpl) PreferenceAvailable() bool { return m.preferences.On }

// PreferenceVerdicts is what core said, before a send's transaction, about
// its recipients in its category (ResolvePreferences).
type PreferenceVerdicts struct {
	category string
	results  map[string]consent.Result
	at       time.Time
}

// of is core's answer about the recipient with sub (nil: not known) and
// address; false when it was not asked about.
func (v *PreferenceVerdicts) of(sub *uuid.UUID, address string) (consent.Result, bool) {
	if v == nil {
		return consent.Result{}, false
	}
	result, ok := v.results[recipientKey(sub, address)]
	return result, ok
}

// recipientKey is how a recipient is asked about: by sub when it is known,
// else by its normalized address.
func recipientKey(sub *uuid.UUID, address string) string {
	if sub != nil {
		return "sub:" + sub.String()
	}
	return "email:" + consent.NormalizeAddress(address)
}

// askedRecipient is the recipient as core is asked about it (recipientKey).
func askedRecipient(sub *uuid.UUID, address string) consent.Recipient {
	if sub != nil {
		return consent.Recipient{Sub: sub.String()}
	}
	return consent.Recipient{Email: consent.NormalizeAddress(address)}
}

// gatedPurpose says a send of purpose is checked: the check is on and the
// send is duyuru. An account template's duyuru send is asked about too;
// its rows, in the hesap lane, are not gated.
func (m *mailerImpl) gatedPurpose(purpose *string) bool {
	p := m.lanes.purposeOf(purpose)
	return m.preferences.On && p != nil && *p == PurposeDuyuru
}

// ResolvePreferences asks core about recipients of a duyuru send of purpose,
// in pages of consent.ResolveBatch, within preferencePageTimeout each and
// preferenceSendTimeout in all. Call it before the transaction the send is
// queued in, and give the answer to Queue.WithPreferences: SkyMail never
// waits for core holding a transaction. Nil, with no error, when the check is
// off or the send is not duyuru; ErrPreferenceUnavailable when core did not
// answer, ErrPreferenceCategoryUnknown for a team core does not know. A
// recipient that brings its own link (a notification's) is not asked about.
func (m *mailerImpl) ResolvePreferences(ctx context.Context, purpose SendPurpose, recipients []RecipientInfo) (*PreferenceVerdicts, error) {
	if !m.gatedPurpose(purpose.Purpose) {
		return nil, nil
	}
	category := clubAnnouncements
	if purpose.Category != nil {
		category = *purpose.Category
	}
	verdicts := &PreferenceVerdicts{category: category, results: map[string]consent.Result{}}
	var asked []consent.Recipient
	var keys []string
	seen := map[string]bool{}
	for _, r := range recipients {
		if r.UnsubscribeURL != "" || strings.TrimSpace(r.Email) == "" {
			continue
		}
		key := recipientKey(r.Sub, r.Email)
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
		asked = append(asked, askedRecipient(r.Sub, r.Email))
	}
	if len(asked) == 0 {
		verdicts.at = m.preferenceCache.now()
		return verdicts, nil
	}
	if m.preferences.Core == nil {
		m.logger.Warn().Msg("Bulk preference check: no core to ask (CORE_URL): the send is refused")
		return nil, ErrPreferenceUnavailable
	}
	started := time.Now()
	all, cancel := context.WithTimeout(ctx, preferenceSendTimeout)
	defer cancel()
	counts := map[string]int{}
	for start := 0; start < len(asked); start += consent.ResolveBatch {
		end := min(start+consent.ResolveBatch, len(asked))
		page, cancelPage := context.WithTimeout(all, preferencePageTimeout)
		results, err := m.preferences.Core.Resolve(page, category, asked[start:end])
		cancelPage()
		if errors.Is(err, consent.ErrCategoryUnknown) {
			return nil, ErrPreferenceCategoryUnknown
		}
		if err != nil {
			m.logger.Warn().Err(err).Int("asked", len(asked)).Dur("took", time.Since(started)).
				Msg("Bulk preference check: core did not say whom the send may go to: nothing queued")
			return nil, ErrPreferenceUnavailable
		}
		for i, result := range results {
			verdicts.results[keys[start+i]] = result
			counts[verdictCount(result)]++
		}
	}
	verdicts.at = m.preferenceCache.now()
	m.preferenceCache.remember(category, verdicts.results, verdicts.at)
	m.logger.Info().Str("event", "bulk_preference_resolve").Int("asked", len(asked)).Int("allowed", counts["allowed"]).
		Int("no_account", counts["no_account"]).Int("opted_out", counts[consent.ReasonOptedOut]).
		Int("inactive", counts[consent.ReasonInactive]).Int("ambiguous", counts[consent.ReasonAmbiguous]).
		Dur("took", time.Since(started)).Msg("Bulk preference check: core answered")
	return verdicts, nil
}

func verdictCount(result consent.Result) string {
	switch {
	case !result.Matched:
		return "no_account"
	case result.Allowed:
		return "allowed"
	}
	return result.Reason
}

// preferenceOutcome is what queueing does with one recipient of a gated
// send: its link, when it was checked, or why it is skipped.
type preferenceOutcome struct {
	link    string
	checked *time.Time
	skip    string
}

// preferenceOf is the outcome of the recipient with sub and address of a
// gated send queued with verdicts. A recipient that was not asked about
// keeps the link it brought, if any (an internal list that changed since it
// was read has none: the dispatcher's safety net then keeps it from going
// out).
func (q *queue) preferenceOf(sub *uuid.UUID, address string) preferenceOutcome {
	result, ok := q.verdicts.of(sub, address)
	switch {
	case !ok:
		return preferenceOutcome{}
	case !result.Matched:
		link, _ := q.preferences.Links.Link(address)
		return preferenceOutcome{link: link}
	case result.Allowed:
		at := q.verdicts.at
		return preferenceOutcome{link: result.UnsubscribeURL, checked: &at}
	}
	return preferenceOutcome{skip: preferenceErrorPrefix + result.Reason}
}

// gated says the rows of a send (one send's) are checked: the check is on,
// the send is duyuru and its mail goes in the duyuru lane (not an account
// template's).
func (q *queue) gated(ctx context.Context, rows []commonMailRow) (bool, error) {
	purpose := q.lanes.purposeOf(rows[0].Purpose)
	if q.preferences == nil || !q.preferences.On || purpose == nil || *purpose != PurposeDuyuru {
		return false, nil
	}
	template, err := q.q.GetMailTaskTemplateKey(ctx, rows[0].TaskID)
	if err != nil {
		return false, fmt.Errorf("read the send's template: %w", err)
	}
	return LaneOf(purpose, template.Key, template.System, q.priority) == LaneDuyuru, nil
}

// preferenceCache is what core said in the dispatcher's turns, shared by the
// dispatcher and the fetch that runs after its transaction.
type preferenceCache struct {
	mu sync.Mutex
	// verdicts: category and recipient key → core's answer, with when.
	verdicts map[string]cachedVerdict
	// openUntil: core failed, and is not asked again before then.
	openUntil time.Time
	// needs: category → recipient key → the recipient, for the next fetch.
	needs map[string]map[string]consent.Recipient
	// gone: the categories core called no team in the last fetch.
	gone     map[string]time.Time
	fetching bool

	ttl, roundTimeout, breakerOpen, fetchDelay time.Duration
	now                                        func() time.Time
}

type cachedVerdict struct {
	result consent.Result
	at     time.Time
}

func newPreferenceCache() *preferenceCache {
	return &preferenceCache{verdicts: map[string]cachedVerdict{}, gone: map[string]time.Time{}, ttl: preferenceCacheTTL,
		roundTimeout: preferenceRoundTimeout, breakerOpen: preferenceBreakerOpen, fetchDelay: preferenceFetchDelay, now: time.Now}
}

func (c *preferenceCache) fresh(at time.Time) bool { return !at.IsZero() && c.now().Sub(at) < c.ttl }

func verdictKey(category, recipient string) string { return category + "\x00" + recipient }

// remember keeps results, read at at, for category: what a send was told
// when it was queued holds for its first turns too.
func (c *preferenceCache) remember(category string, results map[string]consent.Result, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, result := range results {
		c.verdicts[verdictKey(category, key)] = cachedVerdict{result: result, at: at}
	}
	for key, verdict := range c.verdicts {
		if !c.fresh(verdict.at) {
			delete(c.verdicts, key)
		}
	}
}

// checkedRow is a claimed row core gave a verdict about when it was queued.
type checkedRow struct {
	gatedRow
	category  string
	key       string
	recipient consent.Recipient
}

// preferenceVerdict is what the dispatcher decides about the rows of a turn.
type preferenceVerdict struct {
	held    map[uuid.UUID]string
	checked map[time.Time][]uuid.UUID
	waiting []gatedRow
}

// decide sorts the rows by what the cache knows, and notes what it does not.
func (c *preferenceCache) decide(rows []checkedRow) preferenceVerdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := preferenceVerdict{held: map[uuid.UUID]string{}, checked: map[time.Time][]uuid.UUID{}}
	for _, row := range rows {
		if at, ok := c.gone[row.category]; ok && c.fresh(at) {
			v.held[row.id] = preferenceCategoryGoneError
			continue
		}
		verdict, ok := c.verdicts[verdictKey(row.category, row.key)]
		switch {
		case !ok || !c.fresh(verdict.at):
			if c.needs == nil {
				c.needs = map[string]map[string]consent.Recipient{}
			}
			if c.needs[row.category] == nil {
				c.needs[row.category] = map[string]consent.Recipient{}
			}
			c.needs[row.category][row.key] = row.recipient
			v.waiting = append(v.waiting, row.gatedRow)
		// No account any more (an address unverified since): its link and
		// SkyMail's record still apply.
		case !verdict.result.Matched:
		case verdict.result.Allowed:
			v.checked[verdict.at] = append(v.checked[verdict.at], row.id)
		default:
			v.held[row.id] = preferenceErrorPrefix + verdict.result.Reason
		}
	}
	return v
}

// gatePreferences decides (in q's transaction, from the cache alone) about
// the bulk rows this process just took: a duyuru row without a link is
// written failed (the safety net); a row core gave a verdict about is
// decided again, written failed when core now says no, put back when its
// answer is not known yet. It returns the rows to send.
func (m *mailerImpl) gatePreferences(ctx context.Context, q *database.Queries, logger *zerolog.Logger, rows []database.MailQueue) ([]database.MailQueue, error) {
	if !m.preferences.On || len(rows) == 0 {
		return rows, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	found, err := q.GetPreferenceGateClaimedRows(ctx, database.GetPreferenceGateClaimedRowsParams{Ids: ids, ClaimedBy: m.claimant})
	if err != nil {
		return nil, fmt.Errorf("read the rows the bulk preference check applies to: %w", err)
	}
	missing := map[uuid.UUID]string{}
	var checked []checkedRow
	for _, row := range found {
		if LaneOf(m.lanes.purposeOf(row.Purpose), row.Key, row.System, Priority(row.Priority)) != LaneDuyuru {
			continue
		}
		// A required notification type (a ticket, a certificate) may carry
		// no link at all: the safety net is for what may be unsubscribed from.
		if !row.HasLink && !row.RequiredNotification {
			missing[row.ID] = unsubscribeMissingError
			continue
		}
		// Asked about again: a row core gave a verdict about when it was
		// queued, and one that knows its person (a news send's, whose link
		// came from core's audience).
		if (row.PreferenceCheckedAt == nil && row.RecipientSub == nil) || row.Category == nil {
			continue
		}
		gated := gatedRow{id: row.ID, address: consent.NormalizeAddress(row.RecipientEmail), queued: m.preferenceCache.now()}
		if row.CreatedAt != nil {
			gated.queued = *row.CreatedAt
		}
		checked = append(checked, checkedRow{gatedRow: gated, category: *row.Category,
			key: recipientKey(row.RecipientSub, row.RecipientEmail), recipient: askedRecipient(row.RecipientSub, row.RecipientEmail)})
	}
	if len(missing) == 0 && len(checked) == 0 {
		return rows, nil
	}
	if err := m.writePreferenceHeld(ctx, q, logger, missing); err != nil {
		return nil, err
	}
	v := m.preferenceCache.decide(checked)
	if err := m.writePreferenceHeld(ctx, q, logger, v.held); err != nil {
		return nil, err
	}
	c := m.preferenceCache
	c.mu.Lock()
	now, open := c.now(), c.now().Before(c.openUntil)
	c.mu.Unlock()
	if err := m.deferWaiting(ctx, q, logger, v.waiting, waitingRule{
		now: now, open: open, fetchDelay: c.fetchDelay, undecidedAfter: preferenceUndecidedAfter, undecidedError: preferenceUndecidedError,
		undecidedMessage: "Core never said whether recipients may get their duyuru mail: written failed, undecided, never sent",
		waitingMessage:   "Recipients wait for core's preference answer: put back, nothing tried",
	}); err != nil {
		return nil, err
	}
	for at, checkedIDs := range v.checked {
		if _, err := q.SetClaimedRowsPreferenceCheckedAt(ctx, database.SetClaimedRowsPreferenceCheckedAtParams{
			At: at, Ids: checkedIDs, ClaimedBy: m.claimant,
		}); err != nil {
			return nil, fmt.Errorf("note when core allowed the rows: %w", err)
		}
	}
	waiting := make(map[uuid.UUID]bool, len(v.waiting))
	for _, row := range v.waiting {
		waiting[row.id] = true
	}
	kept := make([]database.MailQueue, 0, len(rows))
	for _, row := range rows {
		_, isMissing := missing[row.ID]
		_, isHeld := v.held[row.ID]
		if isMissing || isHeld || waiting[row.ID] {
			continue
		}
		kept = append(kept, row)
	}
	return kept, nil
}

// writePreferenceHeld writes the held rows failed with their reason, nothing
// tried.
func (m *mailerImpl) writePreferenceHeld(ctx context.Context, q *database.Queries, logger *zerolog.Logger, held map[uuid.UUID]string) error {
	byReason := map[string][]uuid.UUID{}
	for id, reason := range held {
		byReason[reason] = append(byReason[reason], id)
	}
	for reason, ids := range byReason {
		if _, err := q.SkipUnsubscribedClaimedRows(ctx, database.SkipUnsubscribedClaimedRowsParams{
			Ids: ids, ClaimedBy: m.claimant, Error: reason,
		}); err != nil {
			return fmt.Errorf("hold rows the bulk preference check stops: %w", err)
		}
		logger.Info().Int("count", len(ids)).Str("reason", reason).
			Msg("Bulk preference check: recipients written failed before their duyuru mail went out, never sent")
	}
	return nil
}

// fetchPreferences starts, unless one is under way or core's breaker is
// open, a fetch of what the last turns found unknown, outside any
// transaction and the dispatcher's turn: one recipients:resolve per
// category (a turn holds far fewer than consent.ResolveBatch rows), all
// within preferenceRoundTimeout. It wakes the dispatcher when it is done.
func (m *mailerImpl) fetchPreferences(ctx context.Context, logger *zerolog.Logger) {
	c := m.preferenceCache
	c.mu.Lock()
	if c.fetching || len(c.needs) == 0 || c.now().Before(c.openUntil) {
		c.mu.Unlock()
		return
	}
	needs := c.needs
	c.needs = nil
	c.fetching = true
	c.mu.Unlock()
	m.preferenceFetches.Add(1)
	go func() {
		defer m.preferenceFetches.Done()
		started := time.Now()
		asked, err := m.fetchVerdicts(ctx, needs)
		c.mu.Lock()
		c.fetching = false
		if err != nil {
			c.openUntil = c.now().Add(c.breakerOpen)
		}
		c.mu.Unlock()
		event := logger.Info()
		if err != nil {
			event = logger.Warn().Err(err).Dur("breaker_open", c.breakerOpen)
		}
		event.Str("event", "bulk_preference_recheck").Bool("ok", err == nil).Int("asked", asked).
			Dur("took", time.Since(started)).Msg("Bulk preference check: asked core again before sending")
		m.wake()
	}()
}

// fetchVerdicts reads what needs names from core, within the round's
// timeout, and keeps it in the cache; it says how many recipients it asked
// about.
func (m *mailerImpl) fetchVerdicts(ctx context.Context, needs map[string]map[string]consent.Recipient) (int, error) {
	if m.preferences.Core == nil {
		return 0, errors.New("no core to ask (CORE_URL)")
	}
	c := m.preferenceCache
	asking, cancel := context.WithTimeout(ctx, c.roundTimeout)
	defer cancel()
	asked := 0
	for category, recipients := range needs {
		keys := make([]string, 0, len(recipients))
		for key := range recipients {
			keys = append(keys, key)
		}
		for start := 0; start < len(keys); start += consent.ResolveBatch {
			batch := keys[start:min(start+consent.ResolveBatch, len(keys))]
			ask := make([]consent.Recipient, len(batch))
			for i, key := range batch {
				ask[i] = recipients[key]
			}
			asked += len(ask)
			results, err := m.preferences.Core.Resolve(asking, category, ask)
			if errors.Is(err, consent.ErrCategoryUnknown) {
				c.mu.Lock()
				c.gone[category] = c.now()
				c.mu.Unlock()
				break
			}
			if err != nil {
				return asked, err
			}
			found := make(map[string]consent.Result, len(batch))
			for i, result := range results {
				found[batch[i]] = result
			}
			c.remember(category, found, c.now())
		}
	}
	return asked, nil
}
