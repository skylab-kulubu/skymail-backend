package handlers

import (
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

// CreateTemplate godoc
//
// @Summary Create a new email template
// @Description Create a new email template with the provided name, HTML content, and plain text content.
// @Tags templates
// @Accept json
// @Produce json
// @Param template body requests.CreateTemplate true "Template details"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /templates [post]
func (h *templateHandlerImpl) CreateTemplate(c fiber.Ctx) error {
	var params requests.CreateTemplate

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	_, err := h.db.CreateTemplate(c.Context(), database.CreateTemplateParams{
		Name:             params.Name,
		HtmlContent:      params.HTMLContent,
		PlainTextContent: params.PlainTextContent,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetTemplates godoc
//
// @Summary List all email templates
// @Description Get a list of all email templates.
// @Tags templates
// @Produce json
// @Success 200 {array} database.Template
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /templates [get]
func (h *templateHandlerImpl) GetTemplates(c fiber.Ctx) error {
	templates, err := h.db.GetAllTemplates(c.Context())
	if err != nil {
		return err
	}

	return c.JSON(templates)
}

// GetTemplate godoc
//
// @Summary Get an email template by ID
// @Description Get details of a specific email template by its ID.
// @Tags templates
// @Produce json
// @Param id path string true "Template ID"
// @Success 200 {object} database.Template
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /templates/{id} [get]
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
// @Summary Update an email template
// @Description Update an existing email template with the provided ID and details.
// @Tags templates
// @Accept json
// @Produce json
// @Param id path string true "Template ID"
// @Param template body requests.UpdateTemplate true "Template details"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /templates/{id} [put]
func (h *templateHandlerImpl) UpdateTemplate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.UpdateTemplate
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	_, err = h.db.UpdateTemplate(c.Context(), database.UpdateTemplateParams{
		ID:               id,
		Name:             params.Name,
		HtmlContent:      params.HTMLContent,
		PlainTextContent: params.PlainTextContent,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// DeleteTemplate godoc
//
// @Summary Delete an email template
// @Description Delete an existing email template by its ID.
// @Tags templates
// @Produce json
// @Param id path string true "Template ID"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /templates/{id} [delete]
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
