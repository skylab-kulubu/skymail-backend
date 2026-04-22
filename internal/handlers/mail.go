package handlers

import (
	"encoding/json"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type MailHandler interface {
	CreateTask(c fiber.Ctx) error
	SendSingle(c fiber.Ctx) error
	GetTasks(c fiber.Ctx) error
	GetTask(c fiber.Ctx) error
	GetTaskQueueItems(c fiber.Ctx) error
}

type mailHandlerImpl struct {
	db     *database.Store
	mailer mailer.Mailer
}

func NewMailHandler(db *database.Store, mailer mailer.Mailer) MailHandler {
	return &mailHandlerImpl{
		db:     db,
		mailer: mailer,
	}
}

// CreateTask godoc
//
//	@Summary		Create a new mail task
//	@Description	Create a new mail task, process template variables, and queue emails for all recipients in the mailing list.
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

	bodyVarsJson, err := json.Marshal(params.BodyVariables)
	if err != nil {
		return err
	}

	// For now, we use a hardcoded sent_by since we don't have auth yet
	sentBy := "admin"

	err = h.mailer.Enqueue(c.Context(), database.CreateMailTaskParams{
		SentBy:        sentBy,
		TemplateID:    &params.TemplateID,
		MailListID:    &params.MailListID,
		BodyVariables: bodyVarsJson,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusCreated)
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

	bodyVarsJson, err := json.Marshal(params.BodyVariables)
	if err != nil {
		return err
	}

	// For now, we use a hardcoded sent_by since we don't have auth yet
	sentBy := "admin"

	err = h.mailer.EnqueueSingle(c.Context(), database.CreateSingleMailTaskParams{
		SentBy:        sentBy,
		TemplateID:    &params.TemplateID,
		BodyVariables: bodyVarsJson,
		Column4:       params.RecipientFullName,
		Column5:       params.RecipientEmail,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusCreated)
}

// GetTasks godoc
//
//	@Summary		List all mail tasks
//	@Description	Get a list of all mail tasks with pagination.
//	@Tags			Mail
//	@Produce		json
//	@Param			_start	query		int	false	"Start index"
//	@Param			_end	query		int	false	"End index"
//	@Success		200		{array}		database.GetAllMailTasksRow
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks [get]
func (h *mailHandlerImpl) GetTasks(c fiber.Ctx) error {
	limit, offset := getPaginationParams(c)

	tasks, err := h.db.GetAllMailTasks(c.Context(), database.GetAllMailTasksParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountMailTasks(c.Context())
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))

	return c.JSON(tasks)
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
