package handlers

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// A send's events (hub ticket skymail-yeni-tasarim B3): GET
// /mail_tasks/{id}/events, the send's life on one line. Who created it, the
// Mail onayı request it came from, when it was queued and to how many,
// every pause, resume and cancel and who did it, the mailer's retries by the
// hour, when it finished with its counts, and the permanent failures
// reported after sending. A pause, a resume, a cancel and a retry are written
// as they happen (mail_task_events); everything else is read from the rows
// that hold it.

// Send event kinds, in the order two events at one instant are told.
const (
	SendEventApprovalSubmitted   = "approval_submitted"
	SendEventApprovalResubmitted = "approval_resubmitted"
	SendEventApprovalEdited      = "approval_edited"
	SendEventApprovalReturned    = "approval_returned"
	SendEventApprovalAccepted    = "approval_accepted"
	SendEventApprovalDeclined    = "approval_declined"
	SendEventApprovalApproved    = "approval_approved"
	SendEventApprovalRejected    = "approval_rejected"
	SendEventApprovalExpired     = "approval_expired"
	SendEventCreated             = "created"
	SendEventQueued              = "queued"
	SendEventPaused              = "paused"
	SendEventResumed             = "resumed"
	SendEventRetried             = "retried"
	SendEventCancelled           = "cancelled"
	SendEventFinished            = "finished"
	SendEventBounced             = "bounced"
)

var sendEventOrder = map[string]int{
	SendEventApprovalSubmitted: 0, SendEventApprovalResubmitted: 1, SendEventApprovalEdited: 2,
	SendEventApprovalReturned: 3, SendEventApprovalAccepted: 4, SendEventApprovalDeclined: 5,
	SendEventApprovalApproved: 6, SendEventApprovalRejected: 7, SendEventApprovalExpired: 8,
	SendEventCreated: 9, SendEventQueued: 10, SendEventPaused: 11, SendEventResumed: 12,
	SendEventRetried: 13, SendEventCancelled: 14, SendEventFinished: 15, SendEventBounced: 16,
}

// Who did a send's event.
const (
	SendActorPerson  = "person"
	SendActorService = "service"
	SendActorSystem  = "system"
)

// sendActorNameBudget is how long GET /mail_tasks/{id}/events waits for
// Keycloak to name the people whose names no row keeps (a send's sender, a
// pause written before 20261125150000); past it their names are null.
const sendActorNameBudget = 2 * time.Second

// skymailName is how SkyMail names itself as a send's sender.
const skymailName = "SkyMail"

// MailTaskEvent is one event of a send.
type MailTaskEvent struct {
	// approval_* (the Mail onayı request the send was queued from, as GET /mail_approvals/{id} tells its history), created, queued, paused, resumed, retried, cancelled, finished or bounced.
	Kind string `json:"kind" enums:"approval_submitted,approval_resubmitted,approval_edited,approval_returned,approval_accepted,approval_declined,approval_approved,approval_rejected,approval_expired,created,queued,paused,resumed,retried,cancelled,finished,bounced"`
	// When it happened. retried: the first retry of the hour; finished: about when the last recipient was answered (its sending, or the attempt that failed it); bounced: the latest report.
	At time.Time `json:"at"`
	// retried: the last retry of the hour; null otherwise.
	Until *time.Time `json:"until"`
	// Who did it; null for what SkyMail did of its own accord (queued, retried, finished, bounced, a pause by the failure rate).
	Actor *MailTaskEventActor `json:"actor"`
	// paused: operator (by hand) or failure_rate (its recipients failed past SKYMAIL_FAILURE_PAUSE_COUNT and SKYMAIL_FAILURE_PAUSE_PERCENT); null otherwise.
	Reason *string `json:"reason" enums:"operator,failure_rate"`
	// An approval event's note: a rejection's reason, or the note an approver or submitter left; null otherwise.
	Note *string `json:"note"`
	// approval_*: the request.
	ApprovalID *uuid.UUID `json:"approval_id"`
	// The numbers the event tells; null on an event that tells none.
	Counts *MailTaskEventCounts `json:"counts"`
}

// MailTaskEventActor is who did a send's event.
type MailTaskEventActor struct {
	// person, service (a service account's token: a login code, a Forms confirmation) or system (SkyMail itself, as the sender of its own notices).
	Kind string `json:"kind" enums:"person,service,system"`
	// The Keycloak subject; null for SkyMail.
	Sub *string `json:"sub"`
	// The name: as the event's record keeps it, or as Keycloak names the account now. "Silinmiş kullanıcı" for an erased account or one Keycloak no longer has; null when Keycloak did not answer in time.
	Name *string `json:"name"`
	// The account was erased or is no longer in Keycloak.
	Deleted bool `json:"deleted"`
}

// MailTaskEventCounts are an event's numbers; only those the event tells are
// present.
type MailTaskEventCounts struct {
	// queued: the recipients queued.
	Recipients *int64 `json:"recipients,omitempty"`
	// finished: sent.
	Sent *int64 `json:"sent,omitempty"`
	// finished: failed for good at the relay (a permanent refusal, or every retry spent).
	Failed *int64 `json:"failed,omitempty"`
	// finished: skipped, never sent: a reserved domain, a suppressed or unsubscribed address, core's no (opted out, inactive, ambiguous).
	Skipped *int64 `json:"skipped,omitempty"`
	// cancelled: the pending recipients the cancel skipped; finished: every recipient skipped by a cancel.
	Cancelled *int64 `json:"cancelled,omitempty"`
	// retried: the retries scheduled in the hour; finished: the recipients tried more than once.
	Retries *int64 `json:"retries,omitempty"`
	// bounced: the recipients a report said do not exist (a hard bounce naming the row's own address), after their mail was sent.
	Bounced *int64 `json:"bounced,omitempty"`
}

// ListTaskEvents godoc
//
//	@Summary		List a send's events
//	@Description	The send's history, oldest first: the Mail onayı request it came from (approval_*), created (who queued it: a person, a service account, or SkyMail), queued (how many recipients), every pause (by hand or by the failure rate), resume and cancel with who did it, the mailer's retries counted by the hour (retried), finished (once no recipient is pending or being sent: sent, failed, skipped, cancelled, retried) and bounced (recipients reported not to exist after sending). Two events at one instant come in that order. Pauses, resumes and retries before 20261125150000 were not recorded: a send paused or cancelled then tells its current pause or cancel only. A send that keeps no record of its recipient (erase_after_send) tells no queued or finished. An erased account reads as Silinmiş kullanıcı. Names no row keeps (a sender's) are read from Keycloak within 2 seconds and are null past that. Every caller with skymail:mails:read reads every send's events. Never cached.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{array}		handlers.MailTaskEvent
//	@Failure		400	{object}	apperrors.Problem	"Bad Request"
//	@Failure		403	{object}	apperrors.Problem	"Forbidden"
//	@Failure		404	{object}	apperrors.Problem	"Not Found"
//	@Failure		500	{object}	apperrors.Problem	"Internal Server Error"
//	@Router			/mail_tasks/{id}/events [get]
func (h *mailHandlerImpl) ListTaskEvents(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	taskID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.ErrBadRequest
	}
	ctx := c.Context()
	facts, err := h.db.GetMailTaskEventFacts(ctx, taskID)
	if err != nil {
		return err
	}
	stored, err := h.db.ListMailTaskEvents(ctx, taskID)
	if err != nil {
		return err
	}
	var approvalEvents []database.MailApprovalEvent
	approval, err := h.db.GetMailTaskApprovalSource(ctx, taskID)
	switch {
	case err == nil:
		if approvalEvents, err = h.db.ListMailApprovalEvents(ctx, approval.ID); err != nil {
			return err
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return err
	}

	names := newActorNames(h, approval)
	events := make([]MailTaskEvent, 0, len(approvalEvents)+len(stored)+5)
	for _, e := range approvalEvents {
		id := e.ApprovalID
		event := MailTaskEvent{Kind: "approval_" + string(e.Kind), At: e.CreatedAt, ApprovalID: &id, Note: e.Note}
		if e.ActorSub != nil {
			event.Actor = names.stored(*e.ActorSub, e.ActorName)
		}
		events = append(events, event)
	}

	events = append(events, MailTaskEvent{Kind: SendEventCreated, At: facts.CreatedAt, Actor: names.sender(facts.SentBy, facts.SentByService)})
	if facts.Recipients > 0 {
		n := facts.Recipients
		events = append(events, MailTaskEvent{Kind: SendEventQueued, At: facts.FirstQueuedAt, Counts: &MailTaskEventCounts{Recipients: &n}})
	}

	recorded := map[string]bool{}
	for _, e := range stored {
		recorded[e.Kind] = true
		event := MailTaskEvent{Kind: e.Kind, At: e.CreatedAt, Reason: e.Reason}
		if e.ActorSub != nil {
			event.Actor = names.stored(*e.ActorSub, e.ActorName)
		}
		switch e.Kind {
		case SendEventRetried:
			until := e.LastAt
			event.Until = &until
			if e.Count != nil {
				n := int64(*e.Count)
				event.Counts = &MailTaskEventCounts{Retries: &n}
			}
		case SendEventCancelled:
			if e.Count != nil {
				n := int64(*e.Count)
				event.Counts = &MailTaskEventCounts{Cancelled: &n}
			}
		}
		events = append(events, event)
	}
	// A pause or a cancel from before mail_task_events: the send's own
	// columns still say who and when.
	if facts.PausedAt != nil && !recorded[SendEventPaused] {
		event := MailTaskEvent{Kind: SendEventPaused, At: *facts.PausedAt, Reason: facts.PausedReason}
		if facts.PausedBy != nil {
			event.Actor = names.lookup(*facts.PausedBy)
		}
		events = append(events, event)
	}
	if facts.CancelledAt != nil && !recorded[SendEventCancelled] {
		event := MailTaskEvent{Kind: SendEventCancelled, At: *facts.CancelledAt}
		if facts.CancelledBy != nil {
			event.Actor = names.lookup(*facts.CancelledBy)
		}
		events = append(events, event)
	}

	if facts.Recipients > 0 && facts.Unfinished == 0 {
		sent, failed, skipped, cancelled, retried := facts.Sent, facts.Failed, facts.Skipped, facts.Cancelled, facts.Retried
		// A cancel's rows keep no claim: the send finished when it was
		// cancelled, if nothing was answered after.
		at := facts.LastOutcomeAt
		if facts.CancelledAt != nil && facts.CancelledAt.After(at) {
			at = *facts.CancelledAt
		}
		events = append(events, MailTaskEvent{Kind: SendEventFinished, At: at, Counts: &MailTaskEventCounts{
			Sent: &sent, Failed: &failed, Skipped: &skipped, Cancelled: &cancelled, Retries: &retried,
		}})
	}
	if facts.Bounced > 0 {
		n := facts.Bounced
		events = append(events, MailTaskEvent{Kind: SendEventBounced, At: facts.LastBouncedAt, Counts: &MailTaskEventCounts{Bounced: &n}})
	}

	names.resolve(ctx)
	for i := range events {
		names.fill(events[i].Actor)
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].At.Equal(events[j].At) {
			return events[i].At.Before(events[j].At)
		}
		return sendEventOrder[events[i].Kind] < sendEventOrder[events[j].Kind]
	})
	return c.JSON(events)
}

// actorNames names the actors of a send's events: a name an event's record
// keeps is used as it is; the others are asked of Keycloak once each, all
// within sendActorNameBudget.
type actorNames struct {
	h        *mailHandlerImpl
	approval database.GetMailTaskApprovalSourceRow
	// Subjects to ask Keycloak for, and what it answered.
	pending map[string]*keycloakName
}

type keycloakName struct {
	name    *string
	deleted bool
	service bool
	asked   bool
}

func newActorNames(h *mailHandlerImpl, approval database.GetMailTaskApprovalSourceRow) *actorNames {
	return &actorNames{h: h, approval: approval, pending: map[string]*keycloakName{}}
}

// stored is an actor whose record keeps the name their token carried.
func (n *actorNames) stored(sub string, name *string) *MailTaskEventActor {
	if sub == database.DeletedUserSubject {
		return deletedActor()
	}
	if name == nil || strings.TrimSpace(*name) == "" {
		return n.lookup(sub)
	}
	s, shown := sub, *name
	return &MailTaskEventActor{Kind: SendActorPerson, Sub: &s, Name: &shown}
}

// sender is who queued the send: SkyMail itself, an approved request's
// submitter as the request names them, or an account Keycloak names.
func (n *actorNames) sender(sentBy string, service *bool) *MailTaskEventActor {
	switch {
	case sentBy == skymailSender:
		name := skymailName
		return &MailTaskEventActor{Kind: SendActorSystem, Name: &name}
	case sentBy == database.DeletedUserSubject:
		return deletedActor()
	case n.approval.SubmitterSub != "" && n.approval.SubmitterSub == sentBy:
		return n.stored(sentBy, n.approval.SubmitterName)
	}
	actor := n.lookup(sentBy)
	if service != nil && *service {
		actor.Kind = SendActorService
	}
	return actor
}

// lookup is an actor whose name Keycloak is asked for (resolve, then fill).
func (n *actorNames) lookup(sub string) *MailTaskEventActor {
	if sub == database.DeletedUserSubject {
		return deletedActor()
	}
	if _, ok := n.pending[sub]; !ok {
		n.pending[sub] = &keycloakName{}
	}
	s := sub
	return &MailTaskEventActor{Kind: SendActorPerson, Sub: &s}
}

// resolve asks Keycloak for every subject lookup took, one after another,
// within sendActorNameBudget. A user Keycloak does not have is a deleted
// account; one it did not answer for in time keeps a null name.
func (n *actorNames) resolve(ctx context.Context) {
	if len(n.pending) == 0 || n.h.kc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendActorNameBudget)
	defer cancel()
	subs := make([]string, 0, len(n.pending))
	for sub := range n.pending {
		subs = append(subs, sub)
	}
	sort.Strings(subs)
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		user, err := n.h.kc.GetUser(ctx, sub)
		if err != nil {
			log.Warn().Err(err).Msg("could not name the actor of a send's event")
			continue
		}
		answer := n.pending[sub]
		answer.asked = true
		if user == nil {
			answer.deleted = true
			continue
		}
		answer.service = user.ServiceAccountClientID != nil && *user.ServiceAccountClientID != ""
		answer.name = userDisplayName(user.FirstName, user.LastName, user.Username)
	}
}

// fill names an actor lookup took, as resolve found it.
func (n *actorNames) fill(actor *MailTaskEventActor) {
	if actor == nil || actor.Sub == nil || actor.Name != nil {
		return
	}
	answer, ok := n.pending[*actor.Sub]
	if !ok || !answer.asked {
		return
	}
	if answer.deleted {
		*actor = *deletedActor()
		return
	}
	actor.Name = answer.name
	if answer.service {
		actor.Kind = SendActorService
	}
}

func deletedActor() *MailTaskEventActor {
	sub, name := database.DeletedUserSubject, database.DeletedUserName
	return &MailTaskEventActor{Kind: SendActorPerson, Sub: &sub, Name: &name, Deleted: true}
}

// userDisplayName is a Keycloak user's first and last name, or their
// username when they have neither; nil when there is none at all.
func userDisplayName(first, last, username *string) *string {
	parts := make([]string, 0, 2)
	for _, p := range []*string{first, last} {
		if p != nil && strings.TrimSpace(*p) != "" {
			parts = append(parts, strings.TrimSpace(*p))
		}
	}
	if len(parts) > 0 {
		name := strings.Join(parts, " ")
		return &name
	}
	if username != nil && strings.TrimSpace(*username) != "" {
		name := strings.TrimSpace(*username)
		return &name
	}
	return nil
}
