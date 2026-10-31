package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type MailHandler interface {
	CreateFormSource(c fiber.Ctx) error
	GetFormSource(c fiber.Ctx) error
	SendFormSource(c fiber.Ctx) error
	CreateTask(c fiber.Ctx) error
	SendSingle(c fiber.Ctx) error
	GetTasks(c fiber.Ctx) error
	GetTask(c fiber.Ctx) error
	GetTaskQueueItems(c fiber.Ctx) error
	GetQueueItemContent(c fiber.Ctx) error
	GetSummary(c fiber.Ctx) error
	GetTrackingMode(c fiber.Ctx) error
	GetTracking(c fiber.Ctx) error
	GetTrackingRecipients(c fiber.Ctx) error
	PauseTask(c fiber.Ctx) error
	ResumeTask(c fiber.Ctx) error
	CancelTask(c fiber.Ctx) error
}

type mailHandlerImpl struct {
	db     *database.Store
	mailer mailer.Mailer
	kc     keycloak.Client
	now    func() time.Time
	// How many days a send's content is shown; 0 is DefaultContentRetentionDays.
	contentRetentionDays int
	formSourceLimit      int
}

func NewMailHandler(db *database.Store, mailer mailer.Mailer, kc keycloak.Client, opts ...MailHandlerOption) MailHandler {
	h := &mailHandlerImpl{db: db, mailer: mailer, kc: kc, now: time.Now}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// errMailAudienceUnavailable: the send names neither an internal list nor a
// Keycloak group Keycloak has. Refused, never queued to nobody.
var errMailAudienceUnavailable = apperrors.New(
	"mail.audience_unavailable",
	"The mailing list is neither an internal list nor a Keycloak group: the group is unknown or gone.",
	fiber.StatusUnprocessableEntity,
)

var errTemplateTargetMissing = apperrors.New(
	"mail.template_target_missing",
	"Either template_id or template_key is required.",
	fiber.StatusBadRequest,
)

// resolveTemplate turns whichever handle the caller used into a template id.
// core passes the uuid it holds in SKYMAIL_WELCOME_TEMPLATE_ID; the Keycloak
// mail provider passes a key so a reseed cannot strand it (ADR-0045).
func (h *mailHandlerImpl) resolveTemplate(c fiber.Ctx, params requests.SendSingleMail) (uuid.UUID, error) {
	if params.TemplateID != uuid.Nil {
		return params.TemplateID, nil
	}

	if params.TemplateKey == "" {
		return uuid.Nil, errTemplateTargetMissing
	}

	key := params.TemplateKey
	template, err := h.db.GetTemplateByKey(c.Context(), &key)
	if err != nil {
		return uuid.Nil, err
	}

	return template.ID, nil
}

// CreatedMailTask is a send just queued.
type CreatedMailTask struct {
	ID uuid.UUID `json:"id"`
	// Whether the send is tracked. A person's send: it asked for open and click tracking and got it — false when it did not ask, when SkyMail's tracking is off (GET /mail_tasks/summary tracking_available), or when its template is a System template or sensitive — or SkyMail tracks every person's send (SKYMAIL_TRACKING_OPERATOR; summary tracking_always): then true whatever it asked and whatever its template. A service's single mail does not ask: true only when SkyMail tracks system mail (SKYMAIL_TRACKING_SYSTEM) and its recipient is tracked (has an account core lets be tracked).
	Tracking bool `json:"tracking"`
	// The send's purpose (ADR 0072 decision 2): islem or duyuru, as written on the send; null while SkyMail's purposes are off (GET /mail_tasks/tracking-mode purpose_available false).
	Purpose *string `json:"purpose" enums:"islem,duyuru"`
	// core's category of a duyuru send; null for any other.
	Category *string `json:"category"`
}

// createdMailTask is the answer to a send just queued.
func createdMailTask(id uuid.UUID, tracking bool, purpose mailer.SendPurpose) CreatedMailTask {
	return CreatedMailTask{ID: id, Tracking: tracking, Purpose: purpose.Purpose, Category: purpose.Category}
}

// operatorTracking is whether a person's send (an operator's mail) of
// templateID to mailListID (nil: to people) is tracked: every one while
// SkyMail tracks every person's send (SKYMAIL_TRACKING_OPERATOR, Yusuf
// 2026-10-21: no box, no opting out per send), whatever it asked and
// whatever its template; otherwise only one that asked, when its template
// may be tracked.
func operatorTracking(ctx context.Context, m mailer.Mailer, asked bool, templateID uuid.UUID, mailListID *uuid.UUID) bool {
	if m.OperatorTrackingAlways() {
		return true
	}
	return asked && m.TrackingApplies(ctx, templateID, mailListID)
}

// CreateTask godoc
//
//	@Summary		Create a new mail task
//	@Description	Create a new mail task and queue emails for all recipients. Supports both internal mailing lists and Keycloak groups. With tracking, each member of a Keycloak group whom core lets be tracked gets a pixel and tracked links; while SkyMail's tracking is off, or for a System or sensitive template (never tracked), the field is ignored and the answer says tracking false. While SkyMail tracks every person's send (SKYMAIL_TRACKING_OPERATOR; summary tracking_always) the field is ignored too and every send is tracked, of any template: a sensitive one with the pixel only, any other with a click address on the links every recipient gets alike (written in the template or in the send's own variables, such as a free body), never on one made from the recipient (.Email, .FullName). Purposes (SKYMAIL_MAIL_PURPOSE; tracking-mode purpose_available): while they are on the send is written with purpose and category (README, "Gönderim amacı ve kategori"); a list or a Keycloak group is always duyuru with core's category (club:duyuru by default), islem is refused with 400 mail_task.purpose_not_allowed.
//	@Tags			Mail
//	@Accept			json
//	@Produce		json
//	@Param			task	body		requests.CreateMailTask	true	"Mail task details"
//	@Success		201		{object}	handlers.CreatedMailTask
//	@Failure		400		{object}	apperrors.AppError	"Bad Request. Purpose, checked in this order, while purposes are on (SKYMAIL_MAIL_PURPOSE): mail_task.purpose_required, mail_task.commercial_content_not_allowed, mail_task.purpose_invalid, then the purpose's own: mail_task.purpose_not_allowed (islem to a list or a group), mail_task.category_required, mail_task.category_invalid, mail_task.commercial_statement_required (SKYMAIL_BULK_PREFERENCE); mail_task.category_unknown: core calls the team category no team"
//	@Failure		422		{object}	apperrors.AppError	"mail.audience_unavailable: the Keycloak group is unknown or gone"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Failure		503		{object}	apperrors.AppError	"mail_task.preference_unavailable (Retry-After): while every duyuru send obeys the recipients' preferences (SKYMAIL_BULK_PREFERENCE; README, \"Toplu postada tercih\"), core did not say whom the send may go to; nothing is queued"
//	@Router			/mail_tasks [post]
func (h *mailHandlerImpl) CreateTask(c fiber.Ctx) error {
	var params requests.CreateMailTask
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	// A list or a Keycloak group is always duyuru.
	purposesOn := h.mailer.PurposeAvailable()
	purpose, err := choosePurpose(purposesOn, h.mailer.PromotionalAvailable(), h.mailer.ListConsentAvailable(), h.mailer.PreferenceAvailable(), params.SendPurpose, targetAudience)
	if err != nil {
		return err
	}
	if err := refuseAccountPromotional(c.Context(), h.db, params.TemplateID, purpose); err != nil {
		return err
	}
	purpose = purposeOrDefault(purposesOn, purpose, duyuruByDefault)
	tracking := operatorTracking(c.Context(), h.mailer, params.Tracking, params.TemplateID, &params.MailListID)
	if _, err := h.db.GetTemplateById(c.Context(), params.TemplateID); err != nil {
		return err
	}

	bodyVarsJson, err := json.Marshal(params.BodyVariables)
	if err != nil {
		return err
	}

	sentBy, ok := c.Locals("user_id").(string)
	if !ok || sentBy == "" {
		return apperrors.ErrForbidden
	}

	_, err = h.db.GetMailingListById(c.Context(), params.MailListID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := rejectArchivedInternalList(c.Context(), h.db, params.MailListID); err != nil {
			return err
		}

		members, err := h.kc.GetGroupMembers(c.Context(), params.MailListID.String())
		if errors.Is(err, keycloak.ErrGroupNotFound) {
			return errMailAudienceUnavailable
		}
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fiber.ErrNotFound
		}

		recipients := make([]mailer.RecipientInfo, 0, len(members))
		for _, m := range members {
			if gocloak.PString(m.Email) == "" {
				continue
			}
			name := strings.TrimSpace(gocloak.PString(m.FirstName) + " " + gocloak.PString(m.LastName))
			if name == "" {
				name = gocloak.PString(m.Username)
			}
			recipients = append(recipients, mailer.RecipientInfo{FullName: name, Email: gocloak.PString(m.Email), Sub: userSub(m)})
		}

		mailListID := params.MailListID
		taskID, err := h.mailer.EnqueueWithRecipients(c.Context(), mailer.EnqueueWithRecipientsParams{
			SentBy:        sentBy,
			TemplateID:    params.TemplateID,
			MailListID:    &mailListID,
			BodyVariables: bodyVarsJson,
			Recipients:    recipients,
			Tracking:      tracking,
			Purpose:       purpose,
		})
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(createdMailTask(taskID, tracking, purpose))
	}

	taskID, err := h.mailer.Enqueue(c.Context(), database.CreateMailTaskParams{
		SentBy:                  sentBy,
		TemplateID:              &params.TemplateID,
		MailListID:              &params.MailListID,
		BodyVariables:           bodyVarsJson,
		TrackingRequested:       tracking,
		Purpose:                 purpose.Purpose,
		Category:                purpose.Category,
		TransactionalStatement:  purpose.TransactionalStatement,
		CommercialContentAbsent: purpose.CommercialContentAbsent,
		OwnEventStatement:       purpose.OwnEventStatement,
	})
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(createdMailTask(taskID, tracking, purpose))
}

// SendSingle godoc
//
//	@Summary		Send a single email
//	@Description	Send a single email to a specific recipient using a template. A person's send may ask for open and click tracking: it then goes in the bulk lane, and the answer says whether it got it (never while SkyMail's tracking is off, or for a System or sensitive template). While SkyMail tracks every person's send (SKYMAIL_TRACKING_OPERATOR; summary tracking_always) the field is ignored: every person's send is tracked, in the bulk lane, of any template, with the pixel and a click address on the links every recipient would get alike — written in the template or in the send's variables, never made from the recipient (.Email, .FullName), though it goes to one person (none at all in a sensitive mail). The recipient's account is found in Keycloak by address before the send is queued (exact primary e-mail, enabled, verified; none is no_account), and core decides for it. A service's token (account and transaction mail) does not ask: its mail stays in the system lane, and while SkyMail tracks system mail (SKYMAIL_TRACKING_SYSTEM) it is tracked the same way, with the pixel and a click address only on the links the template writes out as they are (never on a link a variable fills, such as a reset or verification link; none at all in a sensitive mail); the answer says tracking true when its recipient is tracked. Purposes (SKYMAIL_MAIL_PURPOSE; tracking-mode purpose_available): while they are on the send is written with purpose and category (README, "Gönderim amacı ve kategori"): a person's send may be islem (with transactional_statement true; bilgi@; tracked as any person's send, in the bulk lane then, else the system lane) or duyuru with a category (the bulk lane, duyuru@); without a purpose, duyuru, club:duyuru, tracked or not. A service's token is always islem; its fields are ignored.
//	@Tags			Mail
//	@Accept			json
//	@Produce		json
//	@Param			mail	body		requests.SendSingleMail	true	"Mail details"
//	@Success		201		{object}	handlers.CreatedMailTask
//	@Failure		400		{object}	apperrors.AppError	"Bad Request. A person's purpose, checked in this order, while purposes are on (SKYMAIL_MAIL_PURPOSE): mail_task.purpose_required, mail_task.commercial_content_not_allowed, mail_task.purpose_invalid, then the purpose's own: mail_task.category_not_allowed, mail_task.transactional_statement_required (islem); mail_task.category_required, mail_task.category_invalid, mail_task.commercial_statement_required (duyuru, SKYMAIL_BULK_PREFERENCE); mail_task.category_unknown: core calls the team category no team"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Failure		503		{object}	apperrors.AppError	"mail_task.preference_unavailable (Retry-After): a person's duyuru mail while every duyuru send obeys the recipient's preferences (SKYMAIL_BULK_PREFERENCE), and core did not answer; nothing is queued"
//	@Router			/mail_tasks/single [post]
func (h *mailHandlerImpl) SendSingle(c fiber.Ctx) error {
	var params requests.SendSingleMail
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	templateID, err := h.resolveTemplate(c, params)
	if err != nil {
		return err
	}

	bodyVarsJson, err := json.Marshal(params.BodyVariables)
	if err != nil {
		return err
	}

	sentBy, ok := c.Locals("user_id").(string)
	if !ok || sentBy == "" {
		return apperrors.ErrForbidden
	}

	// Account and transaction mail, which services send here, never asks for
	// tracking: only a person's own send may (every one, with
	// SKYMAIL_TRACKING_OPERATOR on), and it then goes in the bulk lane, an
	// operator's mail. A service's mail stays in the system lane and is
	// tracked only while SKYMAIL_TRACKING_SYSTEM says so.
	service := middlewares.IsServiceAccount(c)
	// A service's mail (account mail, a Forms confirmation, a notice) is
	// always islem: what its body says about a purpose is ignored. A
	// person's may be either, tracked as any person's send.
	purposesOn := h.mailer.PurposeAvailable()
	var purpose mailer.SendPurpose
	if !service {
		if purpose, err = choosePurpose(purposesOn, h.mailer.PromotionalAvailable(), h.mailer.ListConsentAvailable(), h.mailer.PreferenceAvailable(), params.SendPurpose, targetPeople); err != nil {
			return err
		}
		if err := refuseAccountPromotional(c.Context(), h.db, templateID, purpose); err != nil {
			return err
		}
	}
	tracking := !service && operatorTracking(c.Context(), h.mailer, params.Tracking, templateID, nil)
	// A person's send that names no purpose is duyuru (ADR 0072 decision 2:
	// "Seçmezse posta duyurudur"), tracked or not.
	if service {
		purpose = purposeOrDefault(purposesOn, mailer.SendPurpose{}, islemByDefault)
	} else {
		purpose = purposeOrDefault(purposesOn, purpose, duyuruByDefault)
	}
	arg := database.CreateSingleMailTaskParams{
		SentBy:                  sentBy,
		TemplateID:              &templateID,
		BodyVariables:           bodyVarsJson,
		RecipientFullName:       params.RecipientFullName,
		RecipientEmail:          params.RecipientEmail,
		TrackingRequested:       tracking,
		Purpose:                 purpose.Purpose,
		Category:                purpose.Category,
		TransactionalStatement:  purpose.TransactionalStatement,
		CommercialContentAbsent: purpose.CommercialContentAbsent,
	}
	var taskID uuid.UUID
	switch {
	case service && h.mailer.SystemTrackingAvailable():
		taskID, tracking, err = h.mailer.EnqueueSystemSingle(c.Context(), arg, nil)
	case tracking, isDuyuru(purpose):
		// The bulk lane, where tracking is planned: an operator's tracked
		// mail (islem too, which goes out from bilgi@ all the same), or a
		// duyuru single the person did not ask tracking for.
		taskID, err = h.mailer.EnqueueTrackedSingle(c.Context(), arg, nil)
	default:
		taskID, err = h.mailer.EnqueueSingle(c.Context(), arg)
	}
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(createdMailTask(taskID, tracking, purpose))
}

// MailTaskItem is a send as the send list, a send's own page and the home
// screen's recent sends all return it, so a send reads the same on every
// screen. The fields up to mail_list_name are the ones the list and the detail
// have always returned; the rest were added beside them. body_variables is
// null unless content_access is available: the values are the send's content
// too, a code or a link for Keycloak's mails.
type MailTaskItem struct {
	ID              uuid.UUID    `json:"id"`
	SentBy          string       `json:"sent_by"`
	TemplateID      *uuid.UUID   `json:"template_id"`
	MailListID      *uuid.UUID   `json:"mail_list_id"`
	BodyVariables   []byte       `json:"body_variables"`
	CreatedAt       time.Time    `json:"created_at"`
	TemplateName    *string      `json:"template_name"`
	MailListName    *string      `json:"mail_list_name"`
	TemplateKey     *string      `json:"template_key"`
	Status          SendStatus   `json:"status"`
	RecipientCounts QueueTotals  `json:"recipient_counts"`
	Audience        SendAudience `json:"audience"`
	// Whether the caller may open the send's content (GET /mail_tasks/{id}/queue/{itemId}/content), and if not why.
	// masked: a sensitive send, kept with its codes and one-time links masked; pending: a sensitive send not masked yet, not shown; not_kept: a sensitive send emptied (before sensitive content was kept, or not masked for certain).
	ContentAccess ContentAccess `json:"content_access" enums:"available,masked,pending,restricted,expired,not_kept"`
	// When the content sweep empties the send's content; null unless content_access is available or masked.
	ContentExpiresAt *time.Time `json:"content_expires_at"`
	// Whether the send is tracked: it asked for open and click tracking, SkyMail's tracking was on when it was queued and its template may be tracked (not System, not sensitive); or it is a person's send queued while SkyMail tracked every person's send (SKYMAIL_TRACKING_OPERATOR); or it is a service's mail queued while SkyMail tracked system mail (SKYMAIL_TRACKING_SYSTEM).
	Tracking bool `json:"tracking"`
	SendPurposeFields
	// While set, the bulk lane takes none of the send's rows (POST /mail_tasks/{id}/pause, or its failure rate); null otherwise.
	PausedAt *time.Time `json:"paused_at"`
	// operator (paused by hand) or failure_rate (its recipients failed past SKYMAIL_FAILURE_PAUSE_COUNT and SKYMAIL_FAILURE_PAUSE_PERCENT); null when not paused.
	PausedReason *string `json:"paused_reason" enums:"operator,failure_rate"`
	// When the send was cancelled (POST /mail_tasks/{id}/cancel): its pending recipients were skipped; null otherwise.
	CancelledAt *time.Time `json:"cancelled_at"`
}

// SendPurposeFields is a send's purpose as it was written (ADR 0072 decision
// 2), the same on a send and on a Mail onayı request.
type SendPurposeFields struct {
	// islem or duyuru. Null for a send queued while SkyMail's purposes were off, or before them: its stream was read by the rule of before (a list, a group, a Forms source, an approved or a tracked send duyuru; any other islem).
	Purpose *string `json:"purpose" enums:"islem,duyuru"`
	// core's category of a duyuru send (club:duyuru, club:haber, team:<group id>:duyuru|haber); null for any other.
	Category *string `json:"category"`
	// true on an islem send whose sender stated that the mail is about the recipient's own application or transaction; null otherwise (a service's mail, a send from an older client, every duyuru send).
	TransactionalStatement *bool `json:"transactional_statement"`
	// On a duyuru send, the sender's statement that its main purpose is not promotion (a short sponsor thanks or logo is allowed; KVKK notice s6 §5.10), as given; null when none was given, and on every islem send.
	CommercialContentAbsent *bool `json:"commercial_content_absent"`
	// On a duyuru send to a list or a group, the sender's statement that the mail is about the list's own event or work (KVKK notice s6 row 15), kept with the send's sender and time; null otherwise.
	OwnEventStatement *bool `json:"own_event_statement"`
}

// GetTasks godoc
//
//	@Summary		List all mail tasks
//	@Description	Get a list of all mail tasks with pagination, newest first, each with its derived status, recipients by status and audience. The status filter keeps only sends with that derived status; X-Total-Count counts the filtered sends. Keycloak group names are looked up within 2 seconds for the whole page and are null past that.
//	@Tags			Mail
//	@Produce		json
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Param			status	query		string	false	"Derived send status: failed (a recipient failed, other than one skipped: a reserved domain (reserved_domain) or a suppressed address (suppressed); every recipient was skipped; or none was queued a minute after the send), sending (none failed and some pending or processing, or none queued yet within that minute), sent (none failed or queued, some sent)"	Enums(failed,sending,sent)
//	@Success		200		{array}		handlers.MailTaskItem
//	@Header			200		{integer}	X-Total-Count		"Number of sends matching the filter"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks [get]
func (h *mailHandlerImpl) GetTasks(c fiber.Ctx) error {
	status, err := parseSendStatusFilter(c.Query("status"))
	if err != nil {
		return err
	}

	limit, offset := getPaginationParams(c)

	sends, err := h.db.ListMailTaskSends(c.Context(), database.ListMailTaskSendsParams{
		Limit:  limit,
		Offset: offset,
		Status: status,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountMailTaskSends(c.Context(), status)
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(h.mailTaskItems(c.Context(), contentCallerOf(c), sends))
}

// GetTask godoc
//
//	@Summary		Get a mail task by ID
//	@Description	Get details of a specific mail task by its ID, with its derived status, recipients by status and audience, as the send list shows it.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	handlers.MailTaskItem
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/{id} [get]
func (h *mailHandlerImpl) GetTask(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	sends, err := h.db.ListMailTaskSends(c.Context(), database.ListMailTaskSendsParams{
		TaskID: &id,
		Limit:  1,
	})
	if err != nil {
		return err
	}
	if len(sends) == 0 {
		return pgx.ErrNoRows
	}

	return c.JSON(h.mailTaskItems(c.Context(), contentCallerOf(c), sends)[0])
}

// GetTaskQueueItems godoc
//
//	@Summary		Get mail queue items for a task
//	@Description	Get a send's recipients, one queue row each, newest first with the row id breaking ties so pages neither repeat nor skip a recipient. The status filter keeps only recipients in that queue status; X-Total-Count counts the filtered recipients.
//	@Tags			Mail
//	@Produce		json
//	@Param			id		path		string	true	"Task ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Param			status	query		string	false	"Recipient queue status"	Enums(pending,processing,sent,failed)
//	@Success		200		{array}		database.GetMailQueueItemsByTaskIdRow
//	@Header			200		{integer}	X-Total-Count		"Number of recipients matching the filter"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/{id}/queue [get]
func (h *mailHandlerImpl) GetTaskQueueItems(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	status, err := parseRecipientStatusFilter(c.Query("status"))
	if err != nil {
		return err
	}

	limit, offset := getPaginationParams(c)

	items, err := h.db.GetMailQueueItemsByTaskId(c.Context(), database.GetMailQueueItemsByTaskIdParams{
		TaskID: id,
		Limit:  limit,
		Offset: offset,
		Status: status,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountMailQueueItemsByTaskId(c.Context(), database.CountMailQueueItemsByTaskIdParams{
		TaskID: id,
		Status: status,
	})
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}
