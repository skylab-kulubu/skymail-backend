package handlers

import (
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type TemplateHandler interface {
	CreateTemplate(c fiber.Ctx) error
	GetTemplates(c fiber.Ctx) error
	GetTemplate(c fiber.Ctx) error
	UpdateTemplate(c fiber.Ctx) error
	DeleteTemplate(c fiber.Ctx) error
}

type templateHandlerImpl struct {
	db *database.Store
}

func NewTemplateHandler(db *database.Store) TemplateHandler {
	return &templateHandlerImpl{
		db: db,
	}
}

func getPaginationParams(c fiber.Ctx) (int32, int32) {
	startStr := c.Query("_start", "0")
	endStr := c.Query("_end", "10")

	start, _ := strconv.Atoi(startStr)
	end, _ := strconv.Atoi(endStr)

	limit := int32(end - start)
	offset := int32(start)

	if limit <= 0 {
		limit = 10
	}

	return limit, offset
}

// CreateTemplate godoc
//
//	@Summary		Create a new email template
//	@Description	Create a new email template with the provided name, HTML content, and plain text content.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			template	body		requests.CreateTemplate	true	"Template details"
//	@Success		201			{object}	database.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates [post]
func (h *templateHandlerImpl) CreateTemplate(c fiber.Ctx) error {
	var params requests.CreateTemplate

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	template, err := h.db.CreateTemplate(c.Context(), database.CreateTemplateParams{
		Name:              params.Name,
		Subject:           params.Subject,
		HtmlContent:       params.HTMLContent,
		PlainTextContent:  params.PlainTextContent,
		ReactEmailContent: params.ReactEmailContent,
	})
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(template)
}

// GetTemplates godoc
//
//	@Summary		List all email templates
//	@Description	Get a list of all email templates with pagination.
//	@Tags			Templates
//	@Produce		json
//	@Param			_start	query		int	false	"Start index"
//	@Param			_end	query		int	false	"End index"
//	@Success		200		{array}		database.Template
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates [get]
func (h *templateHandlerImpl) GetTemplates(c fiber.Ctx) error {
	limit, offset := getPaginationParams(c)

	templates, err := h.db.GetAllTemplates(c.Context(), database.GetAllTemplatesParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountTemplates(c.Context())
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))

	return c.JSON(templates)
}

// GetTemplate godoc
//
//	@Summary		Get an email template by ID
//	@Description	Get details of a specific email template by its ID.
//	@Tags			Templates
//	@Produce		json
//	@Param			id	path		string	true	"Template ID"
//	@Success		200	{object}	database.Template
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id} [get]
func (h *templateHandlerImpl) GetTemplate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	template, err := h.db.GetTemplateById(c.Context(), id)
	if err != nil {
		return err
	}

	return c.JSON(template)
}

// UpdateTemplate godoc
//
//	@Summary		Update an email template
//	@Description	Update an existing email template with the provided ID and details.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string					true	"Template ID"
//	@Param			template	body		requests.UpdateTemplate	true	"Template details"
//	@Success		200			{object}	database.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		404			{object}	apperrors.AppError	"Not Found"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id} [patch]
func (h *templateHandlerImpl) UpdateTemplate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.UpdateTemplate
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	template, err := h.db.UpdateTemplate(c.Context(), database.UpdateTemplateParams{
		ID:                id,
		Name:              params.Name,
		Subject:           params.Subject,
		HtmlContent:       params.HTMLContent,
		PlainTextContent:  params.PlainTextContent,
		ReactEmailContent: params.ReactEmailContent,
	})
	if err != nil {
		return err
	}

	return c.JSON(template)
}

// DeleteTemplate godoc
//
//	@Summary		Delete an email template
//	@Description	Delete an existing email template by its ID.
//	@Tags			Templates
//	@Produce		json
//	@Param			id	path	string	true	"Template ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id} [delete]
func (h *templateHandlerImpl) DeleteTemplate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	if err := h.db.DeleteTemplate(c.Context(), id); err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}
