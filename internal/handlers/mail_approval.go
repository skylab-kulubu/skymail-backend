package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
// A request goes to a mailing list or to 1..100 people (Yusuf, 2026-09-24);
// the approver decides it once, and each person gets a send of their own.
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
	errApprovalChanged = apperrors.New(
		"mail_approval.changed",
		"The request changed while this action was being prepared. Reload it and try again.",
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
	// variable: a variable's value; template: the template or its version ({id, version_id}); audience: who it goes to ({mail_list_id} or {recipients: [{email, full_name}]}; changes recorded before a request could go to several people hold {recipient_email, recipient_full_name}).
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
	// The send an approval or an acceptance queued; the first, when it queued one per person — the request's task_ids has them all.
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
	// The people it goes to, in the order submitted; empty when it goes to a mailing list. audience reads as a send's: single for one person, whom recipient_email and recipient_full_name name too; people for several.
	Recipients []MailApprovalRecipient `json:"recipients"`
	// The variables it is sent with: as submitted, or as an approver edited them.
	BodyVariables json.RawMessage `json:"body_variables" swaggertype:"object"`
	CreatedAt     time.Time       `json:"created_at"`
	// When it was last submitted; its deadline is seven days on.
	SubmittedAt time.Time `json:"submitted_at"`
	// Pending or returned past this, it expires.
	DeadlineAt time.Time `json:"deadline_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Deprecated: the first of task_ids, until the screens read those (ticket 22). Null until approved.
	TaskID *uuid.UUID `json:"task_id"`
	// The sends, once approved, in order: a list's one, or one per person, task_ids[i] to recipients[i]. Empty until then.
	TaskIDs []uuid.UUID `json:"task_ids"`
	// What happened to it last.
	LastEvent *MailApprovalEvent `json:"last_event"`
}

// MailApprovalRecipient is someone a request goes to, or a preview is
// rendered for.
type MailApprovalRecipient struct {
	// Empty when the submitter knows only the address.
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// MailApprovalPreview is the mail the request would queue, rendered by the
// mailer from the template version it is pinned to: its first person's, or —
// for a list, whose members each get their own — the submitter's, as if they
// were on it.
type MailApprovalPreview struct {
	Subject string `json:"subject"`
	// The whole mail as HTML: operators' template markup with the submitted values. Show it only in a sandboxed iframe (sandbox with no allow-scripts or allow-same-origin), never in the page itself.
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
	// Why it reached no one or fewer than it was for: no_approvers (no one holding skymail:mails:approve has an address), approver_lookup_failed (Keycloak did not say in time), no_address (the submitter's token carried no e-mail), unverified_address (it carried one Keycloak had not verified, which is not used), template_unavailable (the System template is not seeded), enqueue_failed. Null when it reached everyone.
	Problem *string `json:"problem" enums:"no_approvers,approver_lookup_failed,no_address,unverified_address,template_unavailable,enqueue_failed"`
}

// MailApproval is a request whole: as the list shows it, with how many it
// would reach, its preview and whose mail that is (preview_recipient: the
// first person, or for a list the submitter), and everything that happened to
// it. A struct-typed field's comment would become its type's description in
// the OpenAPI document, so preview_recipient is described on Get instead.
type MailApproval struct {
	MailApprovalItem
	// How many it would reach now: its people, a list's members, a Keycloak group's members with an address. Null when Keycloak did not say in time.
	RecipientCount   *int64                `json:"recipient_count"`
	PreviewRecipient MailApprovalRecipient `json:"preview_recipient"`
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
	mailer   mailer.Transactional
	kc       keycloak.Client
	clientID string
	uiURL    string
	now      func() time.Time
}

// NewMailApprovalHandler serves Mail onayı. An approved send is queued with
// mail's Queue, in the approval's own transaction; the notifications go
// through mail's pool methods once the action has committed.
func NewMailApprovalHandler(db *database.Store, mail mailer.Transactional, kc keycloak.Client, opts MailApprovalOptions) MailApprovalHandler {
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
//	@Description	Mail onayı (ADR-0031): a member submits a filled-in send — a template and either a mailing list (an internal list or a Keycloak group, as POST /mail_tasks takes it) or 1..100 people in recipients, each address once whatever its case (each is sent to on their own, as POST /mail_tasks/single sends to one), never both — and nothing is sent until someone holding skymail:mails:approve approves it. Until the screens send recipients, recipient_email and recipient_full_name still submit a send to one person. It is checked as a send would be: the template exists, is not archived and has a published version, which the request is pinned to; the list exists and is not archived, or the Keycloak group exists; every Required variable of the template has a value (FullName and Email are the mailer's); and the template renders with the values, for the first person. It waits seven days; undecided by then, it expires and is never sent.
//	@Description
//	@Description	Every approver — the submitter too, if they hold the role — is mailed the mail.approval-requested System template with a link to the request. That mail is best effort: a submission succeeds whether or not anyone could be told, and notification says how it went.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			send	body		requests.SubmitMailApproval	true	"The send"
//	@Success		201		{object}	handlers.MailApproval		"The request, pending, with notification"
//	@Failure		400		{object}	apperrors.AppError			"validation.error (params.errors: [{field, code, params}]): no template_id; not exactly one of mail_list_id, recipients and recipient_email (mail_list_id, exactly_one_of); more than 100 people (recipients, max_length); a missing or malformed address (recipients[i].email, required or invalid_email); an address twice (recipients[i].email, duplicate, params.first naming the first)"
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
			SubmitterSub:             caller.sub,
			SubmitterName:            caller.name,
			SubmitterEmail:           caller.email,
			SubmitterEmailUnverified: caller.emailUnverified,
			TemplateID:               checked.template.ID,
			TemplateVersionID:        checked.versionID,
			MailListID:               send.mailListID,
			BodyVariables:            send.variables,
			At:                       now,
			DeadlineAt:               now.Add(mailApprovalDeadline),
		})
		if err != nil {
			return err
		}
		id = created.ID
		if err := setRecipients(c.Context(), q, id, send.recipients); err != nil {
			return err
		}
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
//	@Description	An approver (skymail:mails:approve) lists everyone's requests, or only their own with mine=true; anyone else lists only their own. Newest submission first; X-Total-Count counts the filtered requests. A request undecided past its deadline is listed, and filtered, as expired; listing writes nothing and mails no one — the sweep, within a minute, records the expiry and tells the submitter.
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

	now := h.now()
	limit, offset := getPaginationParams(c)
	rows, err := h.db.ListMailApprovals(c.Context(), database.ListMailApprovalsParams{
		Limit: limit, Offset: offset, SubmitterSub: submitter, State: state, AsOf: now,
	})
	if err != nil {
		return err
	}
	count, err := h.db.CountMailApprovals(c.Context(), database.CountMailApprovalsParams{SubmitterSub: submitter, State: state, AsOf: now})
	if err != nil {
		return err
	}

	views := make([]database.GetMailApprovalRow, len(rows))
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		views[i] = database.GetMailApprovalRow(row)
		ids[i] = row.MailApproval.ID
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
		if event, ok := last[view.MailApproval.ID]; ok {
			lastEvent = &event
		}
		items[i] = approvalItem(view, names, lastEvent, now)
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}

// Get godoc
//
//	@Summary		Read a request for approval
//	@Description	A request whole — with how many it would reach, the mail it would queue rendered by the mailer from the template version it is pinned to (preview.html is operator HTML: show it only in a sandboxed iframe), and its history — for an approver or its submitter; to anyone else it is not found. preview_recipient is whose mail the preview is: the first of its people, or for a list the submitter, as if they were on it; it is given also when the preview does not render. A request undecided past its deadline reads as expired; reading writes nothing and mails no one — the sweep, within a minute, records the expiry and tells the submitter.
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
	if !caller.approver && view.MailApproval.SubmitterSub != caller.sub {
		return apperrors.ErrStatusNotFound
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
//	@Description	An approver approves a pending request — their own too, which its history then shows — and it is queued through the send path at once, sent by its submitter: without body_variables exactly as it stands, with them as the approver edited them — the edit is recorded (an edited event naming each variable changed, before and after) and told to the submitter. The send is of the template version the request is pinned to, to its audience as it is now: one send to a mailing list, or one per person (as POST /mail_tasks/single sends to one), all queued in one transaction — all of them or none. Approving is idempotent and race-safe: the request is locked while it is sent, someone else acting on it at that moment is refused (409 mail_approval.busy), and approving an approved request again without an edit sends nothing and answers it as it is.
//	@Description
//	@Description	The submitter is mailed the mail.approval-resolved System template with Decision "approved". A request past its deadline is expired instead (409 mail_approval.expired) and its submitter told.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string							true	"Request ID"
//	@Param			approval	body		requests.ApproveMailApproval	false	"An edit and a note, both optional"
//	@Success		200			{object}	handlers.MailApproval			"The request, approved, with its task_ids and notification"
//	@Failure		400			{object}	apperrors.AppError				"validation.error"
//	@Failure		403			{object}	apperrors.AppError				"Not an approver"
//	@Failure		404			{object}	apperrors.AppError				"No such request"
//	@Failure		409			{object}	apperrors.AppError				"mail_approval.state_conflict (params.state, params.allowed), mail_approval.expired, mail_approval.busy, mail_approval.changed, mail_approval.template_republished (params.submitted_version_id, params.published_version_id), mail_approval.template_unavailable, mail_approval.audience_unavailable or mail_approval.audience_empty"
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
			taskIDs, err := h.send(ctx, q, a, prepared)
			if err != nil {
				return approvalNotice{}, err
			}
			if err := h.decide(ctx, q, a, caller, approvalStep{
				state: database.MailApprovalStateApproved, kind: database.MailApprovalEventKindApproved, note: params.Note, taskID: &taskIDs[0],
			}); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice(decisionApproved, caller, changes, params.Note), nil
		},
	})
}

// Return godoc
//
//	@Summary		Return an edited request to its submitter
//	@Description	An approver edits a pending request and hands it back: the edit is recorded (an edited event naming each variable changed, before and after), the request is returned with a new seven-day deadline — the decision is the submitter's now — and the submitter is mailed the mail.approval-resolved System template with Decision "returned" and the deadline. Nothing is sent. The submitter accepts it — and it goes out as edited — or declines it. body_variables is the whole edit and must differ from the request's.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Request ID"
//	@Param			edit	body		requests.ReturnMailApproval	true	"The edit and a note"
//	@Success		200		{object}	handlers.MailApproval		"The request, returned, with notification"
//	@Failure		400		{object}	apperrors.AppError			"validation.error"
//	@Failure		403		{object}	apperrors.AppError			"Not an approver"
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
			// Returning hands the decision to the submitter, who gets seven
			// days of their own to make it.
			deadline := h.now().Add(mailApprovalDeadline)
			if err := h.decide(ctx, q, a, caller, approvalStep{
				state: database.MailApprovalStateReturned, kind: database.MailApprovalEventKindReturned, note: params.Note, deadline: &deadline,
			}); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice(decisionReturned, caller, changes, params.Note), nil
		},
	})
}

// Reject godoc
//
//	@Summary		Reject a request
//	@Description	An approver refuses a pending request, with a reason. Nothing is sent. The submitter is mailed the mail.approval-resolved System template with Decision "rejected" and the reason as DecisionNote, and may edit and resubmit the request; the rejection stays in its history.
//	@Tags			Mail approval
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string						true	"Request ID"
//	@Param			rejection	body		requests.RejectMailApproval	true	"The reason"
//	@Success		200			{object}	handlers.MailApproval		"The request, rejected, with notification"
//	@Failure		400			{object}	apperrors.AppError			"validation.error: no reason, or a blank one"
//	@Failure		403			{object}	apperrors.AppError			"Not an approver"
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
			if err := h.decide(ctx, q, a, caller, approvalStep{
				state: database.MailApprovalStateRejected, kind: database.MailApprovalEventKindRejected, note: reason,
			}); err != nil {
				return approvalNotice{}, err
			}
			return resolvedNotice(decisionRejected, caller, nil, reason), nil
		},
	})
}

// Accept godoc
//
//	@Summary		Accept an approver's edit and send it
//	@Description	The submitter accepts the edit an approver returned, and the request is sent as edited, exactly as an approval sends it — locked, once, of the pinned template version, one send per person when it goes to people.
//	@Tags			Mail approval
//	@Produce		json
//	@Param			id	path		string					true	"Request ID"
//	@Success		200	{object}	handlers.MailApproval	"The request, approved, with its task_ids"
//	@Failure		403	{object}	apperrors.AppError		"mail_approval.not_submitter"
//	@Failure		404	{object}	apperrors.AppError		"No such request, or not the caller's to see"
//	@Failure		409	{object}	apperrors.AppError		"mail_approval.state_conflict, mail_approval.expired, mail_approval.busy, mail_approval.changed, mail_approval.template_republished, mail_approval.template_unavailable, mail_approval.audience_unavailable or mail_approval.audience_empty"
//	@Failure		500	{object}	apperrors.AppError		"Internal Server Error"
//	@Router			/mail_approvals/{id}/accept [post]
func (h *mailApprovalHandlerImpl) Accept(c fiber.Ctx) error {
	return h.act(c, approvalAction{
		by:    bySubmitter,
		from:  []database.MailApprovalState{database.MailApprovalStateReturned},
		sends: true,
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error) {
			taskIDs, err := h.send(ctx, q, a, prepared)
			if err != nil {
				return approvalNotice{}, err
			}
			return approvalNotice{}, h.decide(ctx, q, a, caller, approvalStep{
				state: database.MailApprovalStateApproved, kind: database.MailApprovalEventKindAccepted, taskID: &taskIDs[0],
			})
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
			return approvalNotice{}, h.decide(ctx, q, a, caller, approvalStep{
				state: database.MailApprovalStateDeclined, kind: database.MailApprovalEventKindDeclined, note: params.Note,
			})
		},
	})
}

// Resubmit godoc
//
//	@Summary		Resubmit a rejected or declined request
//	@Description	The submitter fills a rejected request, or one whose returned edit they declined, in again — whole, as a submission is, its list or its people too — and it is pending again with a new seven-day deadline, pinned to the version its template publishes now. It is checked as a submission is. It stays the same request: a resubmitted event records what changed, and everything before it stays in the history. Every approver is mailed mail.approval-requested again.
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
		check: func(ctx context.Context, view database.GetMailApprovalRow, caller approvalCaller) (checkedSend, error) {
			return h.checkSend(ctx, send, caller.recipient())
		},
		do: func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error) {
			return h.resubmit(ctx, q, a, caller, send, prepared.checked)
		},
	})
}
