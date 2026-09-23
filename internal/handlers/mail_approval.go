package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// Mail onayı (ADR-0031, CONTEXT.md): a send a User without send permission
// submits, held in SkyMail until an approver decides it. The rules are
// Yusuf's (ticket 18):
//
//   - an approver holds MailApproverRole, a client role apart from sending;
//   - a request undecided seven days after it was last submitted expires, its
//     submitter is told, and it never goes out;
//   - a rejection carries a reason; the submitter may edit and resubmit, and
//     the rejection stays in the record;
//   - an approver may edit the variables, then send the edit at once or
//     return it to the submitter, who accepts it — and it goes out — or
//     declines it and may resubmit. Every edit is recorded and told to the
//     submitter; an approval without one sends the request as submitted.
//
// Anyone who can use SkyMail may submit: the one thing that makes a send need
// approval is that its author lacks send permission, which is exactly who the
// ADR is for, and a role of its own would put back the 403 the ADR removes.
const (
	// MailApproverRole is the client role of an approver (ticket 18, decision 1).
	MailApproverRole = "skymail:mails:approve"
	// How long a submission waits for a decision (ticket 18, decision 2).
	mailApprovalDeadline = 7 * 24 * time.Hour
	// All the time a notification gives Keycloak to name the approvers, or a
	// group's name and size, however many calls that takes.
	approvalKeycloakBudget = 5 * time.Second
	// The System templates the notifications are sent with, seeded from
	// skymail-frontend's emails/.
	approvalRequestedKey = "mail.approval-requested"
	approvalResolvedKey  = "mail.approval-resolved"
	// Where the SkyMail UI is when SKYMAIL_UI_URL does not say.
	DefaultSkyMailUIURL = "https://mail.yildizskylab.com"
	// Who sends the mail SkyMail sends of its own accord: an expiry's notice.
	skymailSender = "skymail"
)

var (
	errApprovalTemplateUnavailable = apperrors.New(
		"mail_approval.template_unavailable",
		"The template is unknown or archived, or has no published version to send.",
		fiber.StatusUnprocessableEntity,
	)
	errApprovalAudienceUnavailable = apperrors.New(
		"mail_approval.audience_unavailable",
		"The mailing list is unknown or archived, or the Keycloak group is gone.",
		fiber.StatusUnprocessableEntity,
	)
	errApprovalAudienceEmpty = apperrors.New(
		"mail_approval.audience_empty",
		"The mailing list has no one to send to.",
		fiber.StatusConflict,
	)
	errApprovalVariablesMissing = apperrors.New(
		"mail_approval.required_variables_missing",
		"A Required variable of the template has no value. params.missing names each, with why it is required.",
		fiber.StatusUnprocessableEntity,
	)
	errApprovalUnrenderable = apperrors.New(
		"mail_approval.unrenderable",
		"The template does not render with these variables. params.error is the renderer's message.",
		fiber.StatusUnprocessableEntity,
	)
	errApprovalBusy = apperrors.New(
		"mail_approval.busy",
		"Someone else is acting on this request right now. Reload it and try again.",
		fiber.StatusConflict,
	)
	errApprovalExpired = apperrors.New(
		"mail_approval.expired",
		"The request was undecided for seven days and has expired. It will never be sent.",
		fiber.StatusConflict,
	)
	errApprovalState = apperrors.New(
		"mail_approval.state_conflict",
		"The request is not in a state this action applies to. params.state is its state, params.allowed the states the action applies to.",
		fiber.StatusConflict,
	)
	errApprovalTemplateRepublished = apperrors.New(
		"mail_approval.template_republished",
		"The template has published another version since the request was submitted, so sending it would not send what was submitted. Reject it; the submitter can resubmit it on the version published now.",
		fiber.StatusConflict,
	)
	errApprovalOwnRequest = apperrors.New(
		"mail_approval.own_request",
		"An approver cannot decide a request they submitted.",
		fiber.StatusForbidden,
	)
	errApprovalNotSubmitter = apperrors.New(
		"mail_approval.not_submitter",
		"Only the request's submitter can do this.",
		fiber.StatusForbidden,
	)
	errApprovalNoEdit = apperrors.New(
		"mail_approval.no_edit",
		"The variables are the request's own. An approver returns a request to its submitter only with an edit.",
		fiber.StatusUnprocessableEntity,
	)
)

// atSend is err as a check at send time refuses: the world changed since the
// values were accepted, which is a conflict, not a bad request.
func atSend(err *apperrors.AppError) *apperrors.AppError {
	conflict := *err
	conflict.Status = fiber.StatusConflict
	return &conflict
}

// MailApprovalPerson is someone as their token named them when they acted.
type MailApprovalPerson struct {
	Sub  string  `json:"sub"`
	Name *string `json:"name"`
}

// MailApprovalSubmitter is who submitted a request, and the address their
// token carried, where the decision is mailed.
type MailApprovalSubmitter struct {
	Sub   string  `json:"sub"`
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// MailApprovalTemplate is the template a request sends and the version of it
// that was published when the request was last submitted: the one previewed,
// and the only one it is sent as.
type MailApprovalTemplate struct {
	ID        uuid.UUID `json:"id"`
	VersionID uuid.UUID `json:"version_id"`
	Name      string    `json:"name"`
	Key       *string   `json:"key"`
	// The template has published another version since: approving or accepting is refused (mail_approval.template_republished) until the submitter resubmits.
	Republished bool `json:"republished"`
}

// MailApprovalChange is one thing an edit or a resubmission changed.
type MailApprovalChange struct {
	// variable: a variable's value; template: the template or its version ({id, version_id}); audience: who it goes to ({mail_list_id} or {recipient_email, recipient_full_name}).
	Field string `json:"field" enums:"variable,template,audience"`
	// The variable's name; null for the template and the audience.
	Name *string `json:"name"`
	// The value before; null when there was none.
	Before json.RawMessage `json:"before" swaggertype:"object"`
	// The value after; null when there is none.
	After json.RawMessage `json:"after" swaggertype:"object"`
}

// MailApprovalEvent is one thing that happened to a request.
type MailApprovalEvent struct {
	// 1, 2, 3… in the order they happened.
	Seq int `json:"seq"`
	// submitted, resubmitted and accepted, declined are the submitter's; edited, returned, approved, rejected an approver's; expired SkyMail's.
	Kind string `json:"kind" enums:"submitted,resubmitted,edited,returned,accepted,declined,approved,rejected,expired"`
	// Who did it; null when SkyMail expired the request.
	Actor *MailApprovalPerson `json:"actor"`
	// A rejection's reason, or the note an approver or the submitter left.
	Note *string `json:"note"`
	// What an edit or a resubmission changed; empty for every other event.
	Changes []MailApprovalChange `json:"changes"`
	// The send an approval or an acceptance queued.
	TaskID *uuid.UUID `json:"task_id"`
	At     time.Time  `json:"at"`
}

// MailApprovalItem is a request as the list shows it. Its audience reads as a
// send's does.
type MailApprovalItem struct {
	ID uuid.UUID `json:"id"`
	// pending: awaiting an approver; returned: an approver's edit awaits the submitter; approved: sent; rejected: refused with a reason, the submitter may resubmit; declined: the submitter refused an approver's edit and may resubmit; expired: undecided seven days after it was submitted, never sent.
	State     string                `json:"state" enums:"pending,returned,approved,rejected,declined,expired"`
	Submitter MailApprovalSubmitter `json:"submitter"`
	Template  MailApprovalTemplate  `json:"template"`
	Audience  SendAudience          `json:"audience"`
	// The variables it is sent with: as submitted, or as an approver edited them.
	BodyVariables json.RawMessage `json:"body_variables" swaggertype:"object"`
	CreatedAt     time.Time       `json:"created_at"`
	// When it was last submitted; its deadline is seven days on.
	SubmittedAt time.Time `json:"submitted_at"`
	// Pending or returned past this, it expires.
	DeadlineAt time.Time `json:"deadline_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// The send, once approved.
	TaskID *uuid.UUID `json:"task_id"`
	// What happened to it last.
	LastEvent *MailApprovalEvent `json:"last_event"`
}

// MailApprovalRecipient is who a preview is rendered for.
type MailApprovalRecipient struct {
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// MailApprovalPreview is the mail the request would queue, rendered by the
// mailer from the template version it is pinned to: the single recipient's,
// or — for a list, whose members each get their own — the submitter's, as if
// they were on it.
type MailApprovalPreview struct {
	Subject     string                `json:"subject"`
	HTML        string                `json:"html"`
	PlainText   string                `json:"plain_text"`
	RenderedFor MailApprovalRecipient `json:"rendered_for"`
}

// MailApprovalNotification is the mail an action sent about itself: to every
// approver when a request is submitted, to the submitter when it is decided.
type MailApprovalNotification struct {
	// The System template it was sent with.
	TemplateKey string `json:"template_key" enums:"mail.approval-requested,mail.approval-resolved"`
	// How many people it was queued to.
	Notified int `json:"notified"`
	// Why it reached no one or fewer than it was for: no_approvers (no one else holds skymail:mails:approve), approver_lookup_failed (Keycloak did not say in time), no_address (the submitter's token carried no e-mail), template_unavailable (the System template is not seeded), enqueue_failed. Null when it reached everyone.
	Problem *string `json:"problem" enums:"no_approvers,approver_lookup_failed,no_address,template_unavailable,enqueue_failed"`
}

// MailApproval is a request whole: as the list shows it, with how many it
// would reach, its preview and everything that happened to it.
type MailApproval struct {
	MailApprovalItem
	// How many it would reach now: 1 for one recipient, a list's members, a Keycloak group's members with an address. Null when Keycloak did not say in time.
	RecipientCount *int64 `json:"recipient_count"`
	// Null when it does not render; preview_error says why.
	Preview      *MailApprovalPreview `json:"preview"`
	PreviewError *string              `json:"preview_error"`
	History      []MailApprovalEvent  `json:"history"`
	// On the answer to an action: the mail it sent about itself. Absent when it sent none.
	Notification *MailApprovalNotification `json:"notification,omitempty"`
}

// MailApprovalHandler serves Mail onayı.
type MailApprovalHandler interface {
	Submit(c fiber.Ctx) error
	List(c fiber.Ctx) error
	Get(c fiber.Ctx) error
	Approve(c fiber.Ctx) error
	Return(c fiber.Ctx) error
	Reject(c fiber.Ctx) error
	Accept(c fiber.Ctx) error
	Decline(c fiber.Ctx) error
	Resubmit(c fiber.Ctx) error
	// ExpireDue expires every undecided request past its deadline that no one
	// is acting on, telling each submitter, and returns how many it expired.
	ExpireDue(ctx context.Context) (int, error)
}

// MailApprovalOptions are what the handler needs besides its stores.
type MailApprovalOptions struct {
	// The Keycloak client whose MailApproverRole makes an approver: skymail.
	ClientID string
	// The SkyMail UI the notifications link to; DefaultSkyMailUIURL when empty.
	UIURL string
	// The clock; time.Now when nil.
	Now func() time.Time
}

type mailApprovalHandlerImpl struct {
	db       *database.Store
	mailer   mailer.Mailer
	kc       keycloak.Client
	clientID string
	uiURL    string
	now      func() time.Time
}

func NewMailApprovalHandler(db *database.Store, mail mailer.Mailer, kc keycloak.Client, opts MailApprovalOptions) MailApprovalHandler {
	h := &mailApprovalHandlerImpl{
		db:       db,
		mailer:   mail,
		kc:       kc,
		clientID: opts.ClientID,
		uiURL:    strings.TrimRight(strings.TrimSpace(opts.UIURL), "/"),
		now:      opts.Now,
	}
	if h.uiURL == "" {
		h.uiURL = DefaultSkyMailUIURL
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Submit godoc
//
//	@Summary		Submit a send for approval
//	@Description	Mail onayı (ADR-0031): anyone who can use SkyMail submits a filled-in send — a template and a mailing list (an internal list or a Keycloak group, as POST /mail_tasks takes it) or one recipient (as POST /mail_tasks/single takes them), never both — and nothing is sent until someone holding skymail:mails:approve approves it. It is checked as a send would be: the template exists, is not archived and has a published version, which the request is pinned to; the list exists and is not archived, or the Keycloak group exists; every Required variable of the template has a value (FullName and Email are the mailer's); and the template renders with the values. It waits seven days; undecided by then, it expires and is never sent.
//	@Description
//	@Description	Every approver but the submitter is mailed the mail.approval-requested System template with a link to the request. That mail is best effort: a submission succeeds whether or not anyone could be told, and notification says how it went.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			send	body		requests.SubmitMailApproval	true	"The send"
//	@Success		201		{object}	handlers.MailApproval		"The request, pending, with notification"
//	@Failure		400		{object}	apperrors.AppError			"validation.error: no template_id, a malformed address, or not exactly one of mail_list_id and recipient_email (params.errors)"
//	@Failure		403		{object}	apperrors.AppError			"Forbidden"
//	@Failure		422		{object}	apperrors.AppError			"mail_approval.template_unavailable, mail_approval.audience_unavailable, mail_approval.required_variables_missing (params.missing: [{name, source, reason}]) or mail_approval.unrenderable (params.error)"
//	@Failure		500		{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/mail_approvals [post]
func (h *mailApprovalHandlerImpl) Submit(c fiber.Ctx) error {
	caller, err := approvalCallerOf(c)
	if err != nil {
		return err
	}
	var params requests.SubmitMailApproval
	if err := bindBody(c, &params); err != nil {
		return err
	}
	send, err := approvalSendOf(params)
	if err != nil {
		return err
	}
	checked, err := h.checkSend(c.Context(), send, caller.recipient())
	if err != nil {
		return err
	}

	now := h.now()
	var id uuid.UUID
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		created, err := q.CreateMailApproval(c.Context(), database.CreateMailApprovalParams{
			SubmitterSub:      caller.sub,
			SubmitterName:     caller.name,
			SubmitterEmail:    caller.email,
			TemplateID:        checked.template.ID,
			TemplateVersionID: checked.versionID,
			MailListID:        send.mailListID,
			RecipientEmail:    send.recipientEmail,
			RecipientFullName: send.recipientFullName,
			BodyVariables:     send.variables,
			At:                now,
			DeadlineAt:        now.Add(mailApprovalDeadline),
		})
		if err != nil {
			return err
		}
		id = created.ID
		_, err = q.RecordMailApprovalEvent(c.Context(), database.RecordMailApprovalEventParams{
			ApprovalID: id,
			Kind:       database.MailApprovalEventKindSubmitted,
			ActorSub:   &caller.sub,
			ActorName:  caller.name,
			At:         now,
		})
		return err
	})
	if err != nil {
		return err
	}

	view, err := h.db.GetMailApproval(c.Context(), id)
	if err != nil {
		return err
	}
	notification := h.deliver(c.Context(), view, approvalNotice{kind: noticeRequested, by: caller.sub})
	answer, err := h.approval(c.Context(), view)
	if err != nil {
		return err
	}
	answer.Notification = notification
	return c.Status(fiber.StatusCreated).JSON(answer)
}

// List godoc
//
//	@Summary		List requests for approval
//	@Description	An approver (skymail:mails:approve) lists everyone's requests, or only their own with mine=true; anyone else lists only their own. Newest submission first; X-Total-Count counts the filtered requests. A request past its deadline is expired before it is listed.
//	@Tags			Mail approval
//	@Produce		json
//	@Param			state	query		string	false	"Only requests in this state"	Enums(pending,returned,approved,rejected,declined,expired)
//	@Param			mine	query		bool	false	"Only the caller's own requests"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		handlers.MailApprovalItem
//	@Header			200		{integer}	X-Total-Count		"Number of requests matching the filter"
//	@Failure		400		{object}	apperrors.AppError	"validation.error: an unknown state"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_approvals [get]
func (h *mailApprovalHandlerImpl) List(c fiber.Ctx) error {
	caller, err := approvalCallerOf(c)
	if err != nil {
		return err
	}
	state, err := parseApprovalState(c.Query("state"))
	if err != nil {
		return err
	}
	var submitter *string
	if !caller.approver || c.Query("mine") == "true" {
		submitter = &caller.sub
	}

	if _, err := h.ExpireDue(c.Context()); err != nil {
		log.Warn().Err(err).Msg("could not expire the approval requests past their deadline before listing")
	}

	limit, offset := getPaginationParams(c)
	rows, err := h.db.ListMailApprovals(c.Context(), database.ListMailApprovalsParams{
		Limit: limit, Offset: offset, SubmitterSub: submitter, State: state,
	})
	if err != nil {
		return err
	}
	count, err := h.db.CountMailApprovals(c.Context(), database.CountMailApprovalsParams{SubmitterSub: submitter, State: state})
	if err != nil {
		return err
	}

	views := make([]database.GetMailApprovalRow, len(rows))
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		views[i] = database.GetMailApprovalRow(row)
		ids[i] = row.ID
	}
	lastEvents, err := h.db.ListLastMailApprovalEvents(c.Context(), ids)
	if err != nil {
		return err
	}
	last := make(map[uuid.UUID]MailApprovalEvent, len(lastEvents))
	for _, event := range lastEvents {
		last[event.ApprovalID] = approvalEventOf(event)
	}
	names := h.approvalGroupNames(c.Context(), views)

	items := make([]MailApprovalItem, len(views))
	for i, view := range views {
		var lastEvent *MailApprovalEvent
		if event, ok := last[view.ID]; ok {
			lastEvent = &event
		}
		items[i] = approvalItem(view, names, lastEvent)
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}

// Get godoc
//
//	@Summary		Read a request for approval
//	@Description	A request whole — with how many it would reach, the mail it would queue rendered by the mailer from the template version it is pinned to, and its history — for an approver or its submitter; to anyone else it is not found. A request past its deadline is expired before it is read.
//	@Tags			Mail approval
//	@Produce		json
//	@Param			id	path		string	true	"Request ID"
//	@Success		200	{object}	handlers.MailApproval
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"No such request, or not the caller's to see"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_approvals/{id} [get]
func (h *mailApprovalHandlerImpl) Get(c fiber.Ctx) error {
	caller, err := approvalCallerOf(c)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	view, err := h.db.GetMailApproval(c.Context(), id)
	if err != nil {
		return err
	}
	if !caller.approver && view.SubmitterSub != caller.sub {
		return apperrors.ErrStatusNotFound
	}
	if undecided(view.State) && !h.now().Before(view.DeadlineAt) {
		if err := h.expireIfDue(c.Context(), id); err != nil {
			return err
		}
		if view, err = h.db.GetMailApproval(c.Context(), id); err != nil {
			return err
		}
	}
	answer, err := h.approval(c.Context(), view)
	if err != nil {
		return err
	}
	return c.JSON(answer)
}

// Approve godoc
//
//	@Summary		Approve a request and send it
//	@Description	An approver approves a pending request someone else submitted, and it is queued through the send path at once, sent by its submitter: without body_variables exactly as it stands, with them as the approver edited them — the edit is recorded (an edited event naming each variable changed, before and after) and told to the submitter. The send is of the template version the request is pinned to, to its audience as it is now. Approving is idempotent and race-safe: the request is locked while it is sent, someone else acting on it at that moment is refused (409 mail_approval.busy), and approving an approved request again without an edit sends nothing and answers it as it is.
//	@Description
//	@Description	The submitter is mailed the mail.approval-resolved System template with Decision "approved". A request past its deadline is expired instead (409 mail_approval.expired) and its submitter told.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string							true	"Request ID"
//	@Param			approval	body		requests.ApproveMailApproval	false	"An edit and a note, both optional"
//	@Success		200			{object}	handlers.MailApproval			"The request, approved, with its task_id and notification"
//	@Failure		400			{object}	apperrors.AppError				"validation.error"
//	@Failure		403			{object}	apperrors.AppError				"Not an approver, or mail_approval.own_request"
//	@Failure		404			{object}	apperrors.AppError				"No such request"
//	@Failure		409			{object}	apperrors.AppError				"mail_approval.state_conflict (params.state, params.allowed), mail_approval.expired, mail_approval.busy, mail_approval.template_republished (params.submitted_version_id, params.published_version_id), mail_approval.template_unavailable, mail_approval.audience_unavailable or mail_approval.audience_empty"
//	@Failure		422			{object}	apperrors.AppError				"The edit leaves a Required variable empty (mail_approval.required_variables_missing) or does not render (mail_approval.unrenderable)"
//	@Failure		500			{object}	apperrors.AppError				"Internal Server Error"
//	@Router			/mail_approvals/{id}/approve [post]
func (h *mailApprovalHandlerImpl) Approve(c fiber.Ctx) error {
	var params requests.ApproveMailApproval
	if len(c.Body()) > 0 {
		if err := bindBody(c, &params); err != nil {
			return err
		}
	}
	return h.act(c, approvalAction{
		by:    byApprover,
		from:  []database.MailApprovalState{database.MailApprovalStatePending},
		sends: true,
		repeat: func(a database.MailApproval) bool {
			return a.State == database.MailApprovalStateApproved && params.BodyVariables == nil
		},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error) {
			a, changes, err := h.edit(ctx, q, a, params.BodyVariables, caller)
			if err != nil {
				return approvalNotice{}, err
			}
			taskID, err := h.send(ctx, q, a, prepared)
			if err != nil {
				return approvalNotice{}, err
			}
			if err := h.decide(ctx, q, a, database.MailApprovalStateApproved, database.MailApprovalEventKindApproved, caller, params.Note, &taskID); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice("approved", caller, approvedNote(changes, params.Note)), nil
		},
	})
}

// Return godoc
//
//	@Summary		Return an edited request to its submitter
//	@Description	An approver edits a pending request someone else submitted and hands it back: the edit is recorded (an edited event naming each variable changed, before and after), the request is returned, and the submitter is mailed the mail.approval-resolved System template with Decision "returned". Nothing is sent. The submitter accepts it — and it goes out as edited — or declines it. body_variables is the whole edit and must differ from the request's.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Request ID"
//	@Param			edit	body		requests.ReturnMailApproval	true	"The edit and a note"
//	@Success		200		{object}	handlers.MailApproval		"The request, returned, with notification"
//	@Failure		400		{object}	apperrors.AppError			"validation.error"
//	@Failure		403		{object}	apperrors.AppError			"Not an approver, or mail_approval.own_request"
//	@Failure		404		{object}	apperrors.AppError			"No such request"
//	@Failure		409		{object}	apperrors.AppError			"mail_approval.state_conflict, mail_approval.expired or mail_approval.busy"
//	@Failure		422		{object}	apperrors.AppError			"mail_approval.no_edit, mail_approval.required_variables_missing or mail_approval.unrenderable"
//	@Failure		500		{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/mail_approvals/{id}/return [post]
func (h *mailApprovalHandlerImpl) Return(c fiber.Ctx) error {
	var params requests.ReturnMailApproval
	if err := bindBody(c, &params); err != nil {
		return err
	}
	return h.act(c, approvalAction{
		by:   byApprover,
		from: []database.MailApprovalState{database.MailApprovalStatePending},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, _ approvalPrepared) (approvalNotice, error) {
			a, changes, err := h.edit(ctx, q, a, params.BodyVariables, caller)
			if err != nil {
				return approvalNotice{}, err
			}
			if len(changes) == 0 {
				return approvalNotice{}, errApprovalNoEdit
			}
			if err := h.decide(ctx, q, a, database.MailApprovalStateReturned, database.MailApprovalEventKindReturned, caller, params.Note, nil); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice("returned", caller, returnedNote(changes, params.Note)), nil
		},
	})
}

// Reject godoc
//
//	@Summary		Reject a request
//	@Description	An approver refuses a pending request someone else submitted, with a reason. Nothing is sent. The submitter is mailed the mail.approval-resolved System template with Decision "rejected" and the reason as DecisionNote, and may edit and resubmit the request; the rejection stays in its history.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string						true	"Request ID"
//	@Param			rejection	body		requests.RejectMailApproval	true	"The reason"
//	@Success		200			{object}	handlers.MailApproval		"The request, rejected, with notification"
//	@Failure		400			{object}	apperrors.AppError			"validation.error: no reason, or a blank one"
//	@Failure		403			{object}	apperrors.AppError			"Not an approver, or mail_approval.own_request"
//	@Failure		404			{object}	apperrors.AppError			"No such request"
//	@Failure		409			{object}	apperrors.AppError			"mail_approval.state_conflict, mail_approval.expired or mail_approval.busy"
//	@Failure		500			{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/mail_approvals/{id}/reject [post]
func (h *mailApprovalHandlerImpl) Reject(c fiber.Ctx) error {
	var params requests.RejectMailApproval
	if err := bindBody(c, &params); err != nil {
		return err
	}
	reason := strings.TrimSpace(params.Reason)
	if reason == "" {
		return blankField("reason")
	}
	return h.act(c, approvalAction{
		by:   byApprover,
		from: []database.MailApprovalState{database.MailApprovalStatePending},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, _ approvalPrepared) (approvalNotice, error) {
			if err := h.decide(ctx, q, a, database.MailApprovalStateRejected, database.MailApprovalEventKindRejected, caller, reason, nil); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice("rejected", caller, reason), nil
		},
	})
}

// Accept godoc
//
//	@Summary		Accept an approver's edit and send it
//	@Description	The submitter accepts the edit an approver returned, and the request is sent as edited, exactly as an approval sends it — locked, once, of the pinned template version.
//	@Tags			Mail approval
//	@Produce		json
//	@Param			id	path		string					true	"Request ID"
//	@Success		200	{object}	handlers.MailApproval	"The request, approved, with its task_id"
//	@Failure		403	{object}	apperrors.AppError		"mail_approval.not_submitter"
//	@Failure		404	{object}	apperrors.AppError		"No such request, or not the caller's to see"
//	@Failure		409	{object}	apperrors.AppError		"mail_approval.state_conflict, mail_approval.expired, mail_approval.busy, mail_approval.template_republished, mail_approval.template_unavailable, mail_approval.audience_unavailable or mail_approval.audience_empty"
//	@Failure		500	{object}	apperrors.AppError		"Internal Server Error"
//	@Router			/mail_approvals/{id}/accept [post]
func (h *mailApprovalHandlerImpl) Accept(c fiber.Ctx) error {
	return h.act(c, approvalAction{
		by:    bySubmitter,
		from:  []database.MailApprovalState{database.MailApprovalStateReturned},
		sends: true,
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error) {
			taskID, err := h.send(ctx, q, a, prepared)
			if err != nil {
				return approvalNotice{}, err
			}
			return approvalNotice{}, h.decide(ctx, q, a, database.MailApprovalStateApproved, database.MailApprovalEventKindAccepted, caller, "", &taskID)
		},
	})
}

// Decline godoc
//
//	@Summary		Decline an approver's edit
//	@Description	The submitter refuses the edit an approver returned. Nothing is sent; the submitter may edit and resubmit the request.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string							true	"Request ID"
//	@Param			decline	body		requests.DeclineMailApproval	false	"A note"
//	@Success		200		{object}	handlers.MailApproval			"The request, declined"
//	@Failure		400		{object}	apperrors.AppError				"validation.error"
//	@Failure		403		{object}	apperrors.AppError				"mail_approval.not_submitter"
//	@Failure		404		{object}	apperrors.AppError				"No such request, or not the caller's to see"
//	@Failure		409		{object}	apperrors.AppError				"mail_approval.state_conflict, mail_approval.expired or mail_approval.busy"
//	@Failure		500		{object}	apperrors.AppError				"Internal Server Error"
//	@Router			/mail_approvals/{id}/decline [post]
func (h *mailApprovalHandlerImpl) Decline(c fiber.Ctx) error {
	var params requests.DeclineMailApproval
	if len(c.Body()) > 0 {
		if err := bindBody(c, &params); err != nil {
			return err
		}
	}
	return h.act(c, approvalAction{
		by:   bySubmitter,
		from: []database.MailApprovalState{database.MailApprovalStateReturned},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, _ approvalPrepared) (approvalNotice, error) {
			return approvalNotice{}, h.decide(ctx, q, a, database.MailApprovalStateDeclined, database.MailApprovalEventKindDeclined, caller, params.Note, nil)
		},
	})
}

// Resubmit godoc
//
//	@Summary		Resubmit a rejected or declined request
//	@Description	The submitter fills a rejected request, or one whose returned edit they declined, in again — whole, as a submission is — and it is pending again with a new seven-day deadline, pinned to the version its template publishes now. It is checked as a submission is. It stays the same request: a resubmitted event records what changed, and everything before it stays in the history. Every approver but the submitter is mailed mail.approval-requested again.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Request ID"
//	@Param			send	body		requests.SubmitMailApproval	true	"The send"
//	@Success		200		{object}	handlers.MailApproval		"The request, pending, with notification"
//	@Failure		400		{object}	apperrors.AppError			"validation.error"
//	@Failure		403		{object}	apperrors.AppError			"mail_approval.not_submitter"
//	@Failure		404		{object}	apperrors.AppError			"No such request, or not the caller's to see"
//	@Failure		409		{object}	apperrors.AppError			"mail_approval.state_conflict or mail_approval.busy"
//	@Failure		422		{object}	apperrors.AppError			"mail_approval.template_unavailable, mail_approval.audience_unavailable, mail_approval.required_variables_missing or mail_approval.unrenderable"
//	@Failure		500		{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/mail_approvals/{id}/resubmit [post]
func (h *mailApprovalHandlerImpl) Resubmit(c fiber.Ctx) error {
	var params requests.SubmitMailApproval
	if err := bindBody(c, &params); err != nil {
		return err
	}
	send, err := approvalSendOf(params)
	if err != nil {
		return err
	}
	return h.act(c, approvalAction{
		by:   bySubmitter,
		from: []database.MailApprovalState{database.MailApprovalStateRejected, database.MailApprovalStateDeclined},
		prepare: func(ctx context.Context, view database.GetMailApprovalRow, caller approvalCaller) (approvalPrepared, error) {
			checked, err := h.checkSend(ctx, send, caller.recipient())
			return approvalPrepared{checked: checked}, err
		},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error) {
			return h.resubmit(ctx, q, a, caller, send, prepared.checked)
		},
	})
}
