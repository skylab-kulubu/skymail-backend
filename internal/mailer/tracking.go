package mailer

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
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
}

// WithTracking sets open and click tracking. Without it tracking is off.
func WithTracking(t Tracking) Option {
	return func(m *mailerImpl) { m.tracking = t }
}

// TrackingAvailable reports whether SKYMAIL_TRACKING is on: whether a send
// that asks for tracking gets it.
func (m *mailerImpl) TrackingAvailable() bool { return m.tracking.Config.On() }

// preferencesBudget is all the time a send gives core to say whom it may
// track; past it no one in the send is tracked and the send goes on.
const preferencesBudget = 5 * time.Second

// planTracking decides, for a send that asked for tracking, each queued
// row's token or why it has none (design note 07 §4.1):
//
//   - a single mail (the system lane), or a send of a System template or of
//     one whose content is sensitive (mail_task_content_sensitive): no one,
//     transactional;
//   - a recipient SkyMail knows only by address: no_account;
//   - everyone else as core says, asked once for the send; when core cannot
//     say, no one with an id is tracked (preference_unavailable).
func (q *queue) planTracking(ctx context.Context, taskID uuid.UUID, items []database.CreateMailQueueItemsParams) error {
	skipAll := func(reason string) {
		for i := range items {
			items[i].TrackingSkipReason = &reason
		}
	}
	if q.priority == PrioritySystem {
		skipAll(tracking.SkipTransactional)
		return nil
	}
	exclusion, err := q.q.GetMailTaskTrackingExclusion(ctx, taskID)
	if err != nil {
		return err
	}
	if exclusion.SystemTemplate || exclusion.ContentSensitive {
		skipAll(tracking.SkipTransactional)
		return nil
	}

	var subs []uuid.UUID
	for _, item := range items {
		if item.RecipientSub != nil {
			subs = append(subs, *item.RecipientSub)
		}
	}
	var decisions map[uuid.UUID]tracking.Decision
	if len(subs) > 0 {
		decisions, err = q.resolvePreferences(ctx, subs)
		if err != nil {
			// The people are counted, never named.
			q.logger.Warn().Err(err).Str("event", "tracking_preferences_unavailable").
				Str("task_id", taskID.String()).Int("subjects", len(subs)).
				Msg("Core could not say who may be tracked: no one in the send is tracked, it goes out as usual")
			decisions = nil
		}
	}

	for i := range items {
		reason := tracking.SkipPreferenceUnavailable
		if items[i].RecipientSub == nil {
			reason = tracking.SkipNoAccount
		} else if decision, known := decisions[*items[i].RecipientSub]; known {
			if decision.Allowed {
				token, err := tracking.NewRecipientToken()
				if err != nil {
					return err
				}
				items[i].TrackingToken = token
				continue
			}
			reason = tracking.SkipReasonOf(decision.Reason)
		}
		items[i].TrackingSkipReason = &reason
	}
	return nil
}

// resolvePreferences asks core about subs within preferencesBudget.
func (q *queue) resolvePreferences(ctx context.Context, subs []uuid.UUID) (map[uuid.UUID]tracking.Decision, error) {
	if q.tracking.Preferences == nil {
		return nil, errors.New("tracking preferences: no core client")
	}
	ctx, cancel := context.WithTimeout(ctx, preferencesBudget)
	defer cancel()
	return q.tracking.Preferences.Resolve(ctx, subs)
}

// registerLinks registers the links of a send with a tracked row: the http
// and https links every queued recipient's mail has (tracking.CommonLinks),
// each with a token of its own. A link a variable made personal differs
// between recipients and is never registered, so it is never rewritten.
func (q *queue) registerLinks(ctx context.Context, taskID uuid.UUID, items []database.CreateMailQueueItemsParams) error {
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
	links := tracking.CommonLinks(bodies, q.tracking.Config.BaseURL)
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
