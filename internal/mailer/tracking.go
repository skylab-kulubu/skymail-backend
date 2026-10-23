package mailer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Open and click tracking in the mailer (hub ticket iletisim-servisi 14;
// design note 07 §4.1, §4.4). When a send that asked for it is queued, each
// row gets a token or why it has none, and the links every recipient has are
// registered; when a tracked row goes out, its mail gets the pixel and its
// click addresses on the way to the relay. The mail kept (body_html) is never
// touched: the content panel shows it without a pixel, so looking at a
// recipient's mail never counts as their opening it.

// Tracking is SKYMAIL_TRACKING and what it needs.
type Tracking struct {
	Config tracking.Config
	// Preferences says whom core lets be tracked.
	Preferences tracking.Preferences
	// Accounts finds the account of a person a send names by address only
	// (Keycloak, exact verified primary e-mail).
	Accounts tracking.Accounts
}

// WithTracking sets open and click tracking. Without it tracking is off.
func WithTracking(t Tracking) Option {
	return func(m *mailerImpl) { m.tracking = t }
}

// TrackingAvailable reports whether SKYMAIL_TRACKING is on: whether a send
// that asks for tracking gets it.
func (m *mailerImpl) TrackingAvailable() bool { return m.tracking.Config.On() }

func (m *mailerImpl) PurposeAvailable() bool { return m.lanes.Purpose }

// SystemTrackingAvailable reports whether SKYMAIL_TRACKING_SYSTEM is on with
// SKYMAIL_TRACKING: whether the mail services send without asking is
// tracked (EnqueueSystemSingle, EnqueueWithRecipientsParams.System).
func (m *mailerImpl) SystemTrackingAvailable() bool { return m.tracking.Config.SystemOn() }

// OperatorTrackingAlways reports whether SKYMAIL_TRACKING_OPERATOR is on with
// SKYMAIL_TRACKING: whether every send a person queues is tracked, whether it
// asked or not (the send form offers no box then).
func (m *mailerImpl) OperatorTrackingAlways() bool { return m.tracking.Config.OperatorOn() }

// trackingKind is how a send is tracked.
type trackingKind int

const (
	// trackNone: not at all.
	trackNone trackingKind = iota
	// trackRequested: a person's send that asked for it (the compose form's
	// box), with SKYMAIL_TRACKING on: the bulk lane, a template that is
	// neither System nor sensitive, the links every recipient has.
	trackRequested
	// trackSystem: a service's mail, with SKYMAIL_TRACKING_SYSTEM on
	// (Yusuf, 2026-10-21): whatever its lane and template; the pixel always,
	// a click only on the links its template writes out as they are
	// (tracking.CommonLiteralLinks), none at all in a sensitive mail.
	trackSystem
	// trackOperator: a person's send, with SKYMAIL_TRACKING_OPERATOR on
	// (Yusuf, 2026-10-21: the panel's mail is always tracked; the callers
	// then ask for every one): whatever its template, in the bulk lane only;
	// the pixel always, a click on the links the send gives every recipient
	// alike (linksSendLevel), none at all in a sensitive mail.
	trackOperator
)

// trackingOf is how a send is tracked: a service's mail (system) as the
// system when SKYMAIL_TRACKING_SYSTEM is on, and never otherwise; any other
// send as it asked, when SKYMAIL_TRACKING is on, and under the system's link
// rule when SKYMAIL_TRACKING_OPERATOR is on too.
func (c Tracking) trackingOf(requested, system bool) trackingKind {
	switch {
	case system && c.Config.SystemOn():
		return trackSystem
	case system:
		return trackNone
	case requested && c.Config.OperatorOn():
		return trackOperator
	case requested && c.Config.On():
		return trackRequested
	}
	return trackNone
}

// systemTrackingBudget is all the time a service's mail gives Keycloak and
// core together to say whom it may track: someone is waiting for a login
// code. Past it the mail is not tracked (preference_unavailable) and goes
// out.
const systemTrackingBudget = 2 * time.Second

// TrackingApplies reports whether a send of templateID to mailListID that
// asks for tracking gets it: tracking is on and the template is neither a
// System template nor sensitive (GetTemplateTrackingExclusion). For a send
// to people (mailListID nil) a template that says nothing of its content
// counts as sensitive, as it does for single mail everywhere. It says false
// when that cannot be read. The send's transaction decides again
// (GetMailTaskTrackingExclusion) and, should the template have changed in
// between, says not tracked. With SKYMAIL_TRACKING_OPERATOR on every
// template is tracked (a sensitive one with the pixel only): true.
func (m *mailerImpl) TrackingApplies(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID) bool {
	if m.OperatorTrackingAlways() {
		return true
	}
	if !m.TrackingAvailable() || m.db == nil {
		return false
	}
	trackable, err := m.templateTrackable(ctx, templateID, mailListID)
	return err == nil && trackable
}

// templateTrackable reads whether a send of templateID to mailListID may be
// tracked at all; a template that is not there may not.
func (m *mailerImpl) templateTrackable(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID) (bool, error) {
	exclusion, err := m.db.GetTemplateTrackingExclusion(ctx, database.GetTemplateTrackingExclusionParams{
		TemplateID: templateID,
		Bulk:       mailListID != nil,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !exclusion.SystemTemplate && !exclusion.ContentSensitive, nil
}

// preferencesBudget is all the time a send gives core to say whom it may
// track; past it no one in the send is tracked and the send goes on.
const preferencesBudget = 5 * time.Second

// accountsBudget is all the time a send gives Keycloak to find the accounts
// of the people it names by address, accountLookups of them at once; an
// address not answered within it is not tracked and the send goes on.
const (
	accountsBudget = 5 * time.Second
	accountLookups = 5
)

// TrackingPreferences is what Keycloak and core said, before a send's
// transaction began, about whom the send may track. Nil is no one asked:
// no one is tracked.
type TrackingPreferences struct {
	decisions map[uuid.UUID]tracking.Decision
	// err: core could not say (down, too slow, not the contract's answer).
	err error
	// lookedUp: the send's address-only recipients were looked up in
	// Keycloak. accounts holds, by lower-cased address, each one Keycloak
	// answered for: its account, or nil for none. An address not in it was
	// not answered (Keycloak down or too slow).
	lookedUp bool
	accounts map[string]*uuid.UUID
}

// lookedUpAny says the send's address-only recipients were looked up.
func (p *TrackingPreferences) lookedUpAny() bool { return p != nil && p.lookedUp }

// accountOf is the account found for address, and whether Keycloak was
// asked and answered for it.
func (p *TrackingPreferences) accountOf(address string) (*uuid.UUID, bool) {
	if p == nil || !p.lookedUp {
		return nil, false
	}
	sub, answered := p.accounts[strings.ToLower(strings.TrimSpace(address))]
	return sub, answered
}

// ResolveTracking asks core, within preferencesBudget and outside any
// transaction, whom of recipients — a list's or a group's — a send of
// templateID to mailListID may track. A recipient known only by address is
// not looked up: no_account. No one is asked when tracking is off or the
// template is never tracked (System or sensitive, unless
// SKYMAIL_TRACKING_OPERATOR tracks every template; the send's transaction
// decides that again, and its answer stands). Core failing is an answer
// too: the send tracks no one with an id and goes out.
func (m *mailerImpl) ResolveTracking(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID, recipients []RecipientInfo) *TrackingPreferences {
	return m.resolveTracking(ctx, templateID, mailListID, recipients, false)
}

// ResolvePeopleTracking is ResolveTracking for a send to people named one
// by one (Yusuf, 2026-10-20): a recipient known only by address is looked up
// in Keycloak first (tracking.Accounts: one enabled account with that
// verified primary e-mail), within accountsBudget, accountLookups at once;
// then core is asked
// about everyone with an account. Keycloak failing is an answer too: whom it
// leaves unknown is not tracked (preference_unavailable), and the send goes
// out.
func (m *mailerImpl) ResolvePeopleTracking(ctx context.Context, templateID uuid.UUID, recipients []RecipientInfo) *TrackingPreferences {
	return m.resolveTracking(ctx, templateID, nil, recipients, true)
}

// ResolveSystemTracking is ResolveTracking for a service's mail
// (SKYMAIL_TRACKING_SYSTEM, Yusuf 2026-10-21): no template is left out, a
// recipient known only by address is looked up in Keycloak as for people
// named one by one, and Keycloak and core together get systemTrackingBudget.
// Nil when system tracking is off. Call it before the send's transaction.
func (m *mailerImpl) ResolveSystemTracking(ctx context.Context, recipients []RecipientInfo) *TrackingPreferences {
	if !m.SystemTrackingAvailable() {
		return nil
	}
	subs, addresses := trackingSubjects(recipients, true)
	if len(subs) == 0 && len(addresses) == 0 {
		return &TrackingPreferences{}
	}
	ask, cancel := context.WithTimeout(ctx, systemTrackingBudget)
	defer cancel()
	return m.askTracking(ask, subs, addresses)
}

func (m *mailerImpl) resolveTracking(ctx context.Context, templateID uuid.UUID, mailListID *uuid.UUID, recipients []RecipientInfo, lookUp bool) *TrackingPreferences {
	if !m.TrackingAvailable() {
		return nil
	}
	subs, addresses := trackingSubjects(recipients, lookUp)
	if len(subs) == 0 && len(addresses) == 0 {
		return &TrackingPreferences{}
	}
	if m.OperatorTrackingAlways() {
		return m.askTracking(ctx, subs, addresses)
	}
	if trackable, err := m.templateTrackable(ctx, templateID, mailListID); err == nil && !trackable {
		return &TrackingPreferences{}
	}
	return m.askTracking(ctx, subs, addresses)
}

// trackingSubjects are the ids of recipients and, with lookUp, the
// addresses, lower-cased and each once, of those known only by address.
func trackingSubjects(recipients []RecipientInfo, lookUp bool) ([]uuid.UUID, []string) {
	var subs []uuid.UUID
	var addresses []string
	seen := map[string]bool{}
	for _, r := range recipients {
		if r.Sub != nil {
			subs = append(subs, *r.Sub)
			continue
		}
		address := strings.ToLower(strings.TrimSpace(r.Email))
		if lookUp && address != "" && !seen[address] {
			seen[address] = true
			addresses = append(addresses, address)
		}
	}
	return subs, addresses
}

// askTracking looks addresses up in Keycloak and then asks core about subs
// and the accounts found.
func (m *mailerImpl) askTracking(ctx context.Context, subs []uuid.UUID, addresses []string) *TrackingPreferences {
	prefs := &TrackingPreferences{}
	if len(addresses) > 0 {
		prefs.lookedUp, prefs.accounts = true, m.lookUpAccounts(ctx, addresses)
		for _, sub := range prefs.accounts {
			if sub != nil {
				subs = append(subs, *sub)
			}
		}
	}
	if len(subs) == 0 {
		return prefs
	}
	if m.tracking.Preferences == nil {
		prefs.err = errors.New("tracking preferences: no core client")
		return prefs
	}
	ask, cancel := context.WithTimeout(ctx, preferencesBudget)
	defer cancel()
	prefs.decisions, prefs.err = m.tracking.Preferences.Resolve(ask, subs)
	return prefs
}

// lookUpAccounts asks Keycloak for the account of each address, at most
// accountLookups at once and all within accountsBudget, and returns the
// answers by address. An address Keycloak did not answer for is left out.
// Addresses are counted in the log, never written.
func (m *mailerImpl) lookUpAccounts(ctx context.Context, addresses []string) map[string]*uuid.UUID {
	accounts := make(map[string]*uuid.UUID, len(addresses))
	if m.tracking.Accounts == nil {
		m.logger.Warn().Str("event", "tracking_accounts_unavailable").Str("cause", "no_keycloak").Int("addresses", len(addresses)).
			Msg("No Keycloak to find the accounts of people named by address: they are not tracked, the send goes on")
		return accounts
	}
	ask, cancel := context.WithTimeout(ctx, accountsBudget)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, accountLookups)
	failed := 0
	for _, address := range addresses {
		wg.Add(1)
		go func(address string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ask.Done():
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			sub, err := m.tracking.Accounts.VerifiedAccountByEmail(ask, address)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			accounts[address] = sub
		}(address)
	}
	wg.Wait()
	if failed > 0 {
		m.logger.Warn().Str("event", "tracking_accounts_unavailable").Str("cause", "keycloak").
			Int("addresses", len(addresses)).Int("unanswered", failed).
			Msg("Keycloak did not say whose accounts some addresses are: they are not tracked, the send goes on")
	}
	return accounts
}

// linkRule is which of a tracked send's links a click is counted on.
type linkRule int

const (
	// linksNone: none; every link goes to its address as it is.
	linksNone linkRule = iota
	// linksCommon: those every recipient's mail has (tracking.CommonLinks);
	// a person's send.
	linksCommon
	// linksLiteral: of those, only the ones the template writes out as they
	// are (tracking.CommonLiteralLinks); a service's mail. A link a variable
	// fills — a password reset, an e-mail verification or a login link, an
	// address with a sensitive variable — is never rewritten (Yusuf,
	// 2026-10-21; SendGrid's clicktracking=off and Postmark's
	// data-pm-no-track for the same links).
	linksLiteral
	// linksSendLevel: of those, the ones the send gives every recipient
	// alike: written out in the template, or in a send's own variable — the
	// body an operator wrote in free.basic, an address typed once for all —
	// but never one that changes with the recipient (.Email, .FullName,
	// .UnsubscribeUrl, in whole or in part). A person's send with
	// SKYMAIL_TRACKING_OPERATOR on (orchestrator, 2026-10-21, as Mailchimp,
	// HubSpot and Dynamics count every link of a campaign's body but the
	// ones marked). It is read by rendering the mail again for two made-up
	// recipients (operatorProbes): a link both renders and every real
	// recipient's mail carry depends on nothing about the recipient. So a
	// one-person send never counts the link its recipient's address made.
	linksSendLevel
)

// planTracking decides, in the send's transaction, for a send that asked for
// tracking or a service's mail tracked by the system (kind), each queued
// row's token or why it has none (design note 07 §4.1), and says which links
// a click is counted on:
//
//   - a send that asked (trackRequested): a mail in the system lane, or a
//     send of a System template or of one whose content is sensitive
//     (mail_task_content_sensitive), is not tracked at all,
//     tracking_requested false and no row reasons, as TrackingApplies told
//     the caller; otherwise the links every recipient has (linksCommon);
//   - a service's mail (trackSystem): tracked in any lane and of any
//     template. Sensitive content (a code, a one-time link) gets the pixel
//     and no click address at all (linksNone): what its links carry is what
//     masking hides, and retention empties a sensitive send's links (D7).
//     Any other gets one only on the links its template writes out as they
//     are (linksLiteral);
//   - a person's send with SKYMAIL_TRACKING_OPERATOR on (trackOperator):
//     of any template, the pixel only when it is sensitive, otherwise a click
//     on the links every recipient gets alike (linksSendLevel); in the bulk
//     lane only: in the system lane it is not tracked at all, as a send that
//     asked is not.
//     So a tracked mail in the system lane is always one the system sends
//     (a service's, or SkyMail's own approval notice), which the person
//     report reads with the role only (GetTrackingRecipients);
//   - a recipient SkyMail knows only by address: no_account — an internal
//     list's, or a person a send names one by one for whom Keycloak found no
//     account (preferences.accounts). Such a person Keycloak did not answer
//     for: preference_unavailable;
//   - everyone else as core said before the transaction (preferences). Core
//     not asked, failing, or not asked about this person (the recipients
//     changed since): preference_unavailable. Core is never asked here.
func (q *queue) planTracking(ctx context.Context, taskID uuid.UUID, items []database.CreateMailQueueItemsParams, preferences *TrackingPreferences, kind trackingKind) (linkRule, error) {
	rule := linksCommon
	switch kind {
	case trackNone:
		return linksNone, nil
	case trackOperator, trackSystem:
		if kind == trackOperator && q.priority == PrioritySystem {
			return linksNone, q.q.ClearMailTaskTracking(ctx, taskID)
		}
		exclusion, err := q.q.GetMailTaskTrackingExclusion(ctx, taskID)
		if err != nil {
			return linksNone, err
		}
		rule = linksLiteral
		if kind == trackOperator {
			rule = linksSendLevel
		}
		if exclusion.ContentSensitive {
			rule = linksNone
		}
	default:
		if q.priority == PrioritySystem {
			return linksNone, q.q.ClearMailTaskTracking(ctx, taskID)
		}
		exclusion, err := q.q.GetMailTaskTrackingExclusion(ctx, taskID)
		if err != nil {
			return linksNone, err
		}
		if exclusion.SystemTemplate || exclusion.ContentSensitive {
			return linksNone, q.q.ClearMailTaskTracking(ctx, taskID)
		}
	}

	var decisions map[uuid.UUID]tracking.Decision
	subjects := 0
	for _, item := range items {
		if item.RecipientSub != nil {
			subjects++
		}
	}
	switch {
	case subjects == 0:
	case preferences == nil:
		q.logger.Warn().Str("event", "tracking_preferences_unavailable").Str("cause", "not_asked").
			Str("task_id", taskID.String()).Int("subjects", subjects).
			Msg("Core was not asked who may be tracked: no one in the send is tracked, it goes out as usual")
	case preferences.err != nil:
		// The people are counted, never named.
		q.logger.Warn().Err(preferences.err).Str("event", "tracking_preferences_unavailable").Str("cause", "core").
			Str("task_id", taskID.String()).Int("subjects", subjects).
			Msg("Core could not say who may be tracked: no one in the send is tracked, it goes out as usual")
	default:
		decisions = preferences.decisions
	}

	for i := range items {
		reason := tracking.SkipPreferenceUnavailable
		if items[i].RecipientSub == nil {
			if _, answered := preferences.accountOf(items[i].RecipientEmail); answered || !preferences.lookedUpAny() {
				reason = tracking.SkipNoAccount
			}
		} else if decision, known := decisions[*items[i].RecipientSub]; known {
			if decision.Allowed {
				token, err := tracking.NewRecipientToken()
				if err != nil {
					return linksNone, err
				}
				items[i].TrackingToken = token
				continue
			}
			reason = tracking.SkipReasonOf(decision.Reason)
		}
		items[i].TrackingSkipReason = &reason
	}
	return rule, nil
}

// probeRecipients are the two made-up recipients linksSendLevel renders a
// send's mail for: they differ in every field a mail takes from its
// recipient, and no real address or link is theirs (.invalid and .example,
// RFC 2606). Each field differs in length, first and last character, and an
// e-mail in its local part and domain too, so that what a template makes of
// a field ({{len .Email}}, {{slice .Email 0 1}}, {{printf "%.3s" .FullName}})
// differs between them as well, and a link made from it is never common
// (hub review T8). FullName B is not ASCII: its byte and rune counts differ.
var probeRecipients = [2]RecipientInfo{
	{FullName: "Sonda", Email: "a@sonda.invalid", UnsubscribeURL: "https://a.invalid/u"},
	{FullName: "Örnek Yoklama Alıcısı B Ç", Email: "zz.yoklama-bbbbbbbbbbbb@skymail-probe-b.example", UnsubscribeURL: "https://skymail-probe-b.example/abonelik/bbbbbbbbbbbb?x=1"},
}

// operatorProbes is a send's HTML rendered with its variables for each of
// probeRecipients, the mails linksSendLevel compares the real ones with; nil
// when either does not render, and then only the template's own links count.
func (t mailTemplates) operatorProbes(vars map[string]interface{}) []string {
	probes := make([]string, 0, len(probeRecipients))
	for _, recipient := range probeRecipients {
		rendered, err := t.render(vars, recipient)
		if err != nil {
			return nil
		}
		probes = append(probes, rendered.HTML)
	}
	return probes
}

// registerLinks registers the links of a send with a tracked row, each with
// a token of its own, as rule says: the http and https links every queued
// recipient's mail has (tracking.CommonLinks; a link a variable made
// personal differs between recipients and is never registered, so it is
// never rewritten); for a service's mail only those of them templateHTML
// writes out as they are (tracking.CommonLiteralLinks); for a person's send
// with SKYMAIL_TRACKING_OPERATOR on those the probes carry too (probes, from
// operatorProbes), or, without probes, the template's own.
func (q *queue) registerLinks(ctx context.Context, taskID uuid.UUID, items []database.CreateMailQueueItemsParams, rule linkRule, templateHTML string, probes []string) error {
	if rule == linksNone {
		return nil
	}
	tracked := false
	bodies := make([]string, 0, len(items))
	for _, item := range items {
		tracked = tracked || item.TrackingToken != nil
		if item.BodyHtml != nil {
			bodies = append(bodies, *item.BodyHtml)
		}
	}
	if !tracked {
		return nil
	}
	var links []string
	base := q.tracking.Config.BaseURL
	switch {
	case rule == linksSendLevel && len(probes) == len(probeRecipients):
		links = tracking.CommonLinks(append(append(make([]string, 0, len(bodies)+len(probes)), bodies...), probes...), base)
	case rule == linksSendLevel, rule == linksLiteral:
		links = tracking.CommonLiteralLinks(bodies, templateHTML, base)
	default:
		links = tracking.CommonLinks(bodies, base)
	}
	if len(links) == 0 {
		return nil
	}
	params := make([]database.CreateMailTaskLinksParams, len(links))
	for i, link := range links {
		token, err := tracking.NewLinkToken()
		if err != nil {
			return err
		}
		url := link
		params[i] = database.CreateMailTaskLinksParams{TaskID: taskID, LinkToken: token, Url: &url, Position: int16(i)}
	}
	_, err := q.q.CreateMailTaskLinks(ctx, params)
	return err
}

// outgoingHTML is the HTML of job's mail as it goes to the relay: as kept,
// unless the row is tracked and its token open. Then, with tracking on, it
// carries the pixel and its links' click addresses (tracking.Decorate); with
// tracking off it goes out as kept and its token is closed (flag_off). A
// tracked mail whose links cannot be read goes out as kept: never tracked
// by halves.
func (s *emailSender) outgoingHTML(ctx context.Context, job database.MailQueue) string {
	body := *job.BodyHtml
	if len(job.TrackingToken) == 0 || job.TrackingDisabledAt != nil || s.db == nil {
		return body
	}
	if !s.tracking.Config.On() {
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
		defer cancel()
		if _, err := s.db.CloseQueueTrackingFlagOff(closing, job.ID); err != nil {
			s.logger.Warn().Err(err).Str("job_id", job.ID.String()).
				Msg("Tracking is off: the tracked mail goes out clean; its token could not be closed")
		}
		return body
	}
	links, err := s.db.ListMailTaskLinkTargets(ctx, job.TaskID)
	if err != nil {
		s.logger.Warn().Err(err).Str("job_id", job.ID.String()).
			Msg("The tracked mail's links could not be read: it goes out without tracking")
		return body
	}
	base := s.tracking.Config.BaseURL
	clicks := make(map[string]string, len(links))
	for _, link := range links {
		clicks[link.Url] = tracking.ClickURL(base, job.TrackingToken, link.LinkToken)
	}
	return tracking.Decorate(body, tracking.PixelURL(base, job.TrackingToken), clicks)
}
