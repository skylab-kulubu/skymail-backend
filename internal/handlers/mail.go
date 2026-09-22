package handlers

import (
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
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type MailHandler interface {
	CreateTask(c fiber.Ctx) error
	SendSingle(c fiber.Ctx) error
	GetTasks(c fiber.Ctx) error
	GetTask(c fiber.Ctx) error
	GetTaskQueueItems(c fiber.Ctx) error
	GetSummary(c fiber.Ctx) error
}

type mailHandlerImpl struct {
	db     *database.Store
	mailer mailer.Mailer
	kc     keycloak.Client
	now    func() time.Time
}

func NewMailHandler(db *database.Store, mailer mailer.Mailer, kc keycloak.Client) MailHandler {
	return &mailHandlerImpl{db: db, mailer: mailer, kc: kc, now: time.Now}
}

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

// CreateTask godoc
//
//	@Summary		Create a new mail task
//	@Description	Create a new mail task and queue emails for all recipients. Supports both internal mailing lists and Keycloak groups.
//	@Tags			Mail
//	@Accept			json
//	@Produce		json
//	@Param			task	body	requests.CreateMailTask	true	"Mail task details"
//	@Success		201		"Created"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks [post]
func (h *mailHandlerImpl) CreateTask(c fiber.Ctx) error {
	var params requests.CreateMailTask
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
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
			recipients = append(recipients, mailer.RecipientInfo{FullName: name, Email: gocloak.PString(m.Email)})
		}

		mailListID := params.MailListID
		taskID, err := h.mailer.EnqueueWithRecipients(c.Context(), mailer.EnqueueWithRecipientsParams{
			SentBy:        sentBy,
			TemplateID:    params.TemplateID,
			MailListID:    &mailListID,
			BodyVariables: bodyVarsJson,
			Recipients:    recipients,
		})
		if err != nil {
			return err
		}
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": taskID})
	}

	taskID, err := h.mailer.Enqueue(c.Context(), database.CreateMailTaskParams{
		SentBy:        sentBy,
		TemplateID:    &params.TemplateID,
		MailListID:    &params.MailListID,
		BodyVariables: bodyVarsJson,
	})
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": taskID})
}

// SendSingle godoc
//
//	@Summary		Send a single email
//	@Description	Send a single email to a specific recipient using a template.
//	@Tags			Mail
//	@Accept			json
//	@Produce		json
//	@Param			mail	body	requests.SendSingleMail	true	"Mail details"
//	@Success		201		"Created"
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
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

	taskID, err := h.mailer.EnqueueSingle(c.Context(), database.CreateSingleMailTaskParams{
		SentBy:            sentBy,
		TemplateID:        &templateID,
		BodyVariables:     bodyVarsJson,
		RecipientFullName: params.RecipientFullName,
		RecipientEmail:    params.RecipientEmail,
	})
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": taskID})
}

// MailTaskListItem is a row of the send list. The fields up to mail_list_name
// are the ones the list has always returned; status and recipient_counts were
// added beside them.
type MailTaskListItem struct {
	ID              uuid.UUID   `json:"id"`
	SentBy          string      `json:"sent_by"`
	TemplateID      *uuid.UUID  `json:"template_id"`
	MailListID      *uuid.UUID  `json:"mail_list_id"`
	BodyVariables   []byte      `json:"body_variables"`
	CreatedAt       time.Time   `json:"created_at"`
	TemplateName    *string     `json:"template_name"`
	MailListName    *string     `json:"mail_list_name"`
	Status          SendStatus  `json:"status"`
	RecipientCounts QueueCounts `json:"recipient_counts"`
}

// GetTasks godoc
//
//	@Summary		List all mail tasks
//	@Description	Get a list of all mail tasks with pagination, newest first, each with its derived status and recipients by status. The status filter keeps only sends with that derived status; X-Total-Count counts the filtered sends.
//	@Tags			Mail
//	@Produce		json
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Param			status	query		string	false	"Derived send status: failed (at least one recipient failed), sending (none failed, some pending or processing), sent (none failed or queued, some sent), empty (no recipients)"	Enums(failed,sending,sent,empty)
//	@Success		200		{array}		handlers.MailTaskListItem
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

	items := make([]MailTaskListItem, len(sends))
	for i, send := range sends {
		items[i] = MailTaskListItem{
			ID:              send.ID,
			SentBy:          send.SentBy,
			TemplateID:      send.TemplateID,
			MailListID:      send.MailListID,
			BodyVariables:   send.BodyVariables,
			CreatedAt:       send.CreatedAt,
			TemplateName:    send.TemplateName,
			MailListName:    send.MailListName,
			Status:          SendStatus(send.Status),
			RecipientCounts: recipientCounts(send),
		}
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}

// GetTask godoc
//
//	@Summary		Get a mail task by ID
//	@Description	Get details of a specific mail task by its ID.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	database.GetMailTaskByIdRow
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/{id} [get]
func (h *mailHandlerImpl) GetTask(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	task, err := h.db.GetMailTaskById(c.Context(), id)
	if err != nil {
		return err
	}

	return c.JSON(task)
}

// GetTaskQueueItems godoc
//
//	@Summary		Get mail queue items for a task
//	@Description	Get a list of mail queue items associated with a specific task ID, with pagination.
//	@Tags			Mail
//	@Produce		json
//	@Param			id		path		string	true	"Task ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		database.MailQueue
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/{id}/queue [get]
func (h *mailHandlerImpl) GetTaskQueueItems(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	limit, offset := getPaginationParams(c)

	items, err := h.db.GetMailQueueItemsByTaskId(c.Context(), database.GetMailQueueItemsByTaskIdParams{
		TaskID: id,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountMailQueueItemsByTaskId(c.Context(), id)
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}
