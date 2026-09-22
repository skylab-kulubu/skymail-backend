package handlers

import (
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// A system template is addressed by key by another service (the Keycloak mail
// provider, core), so archiving one would silently break that service's mail.
// ADR-0045: system templates are protected from archiving.
var errSystemTemplateArchive = apperrors.New(
	"template.system_protected",
	"System templates cannot be archived.",
	fiber.StatusConflict,
)

var errSystemTemplateKey = apperrors.New(
	"template.system_key_immutable",
	"The key of a system template cannot be changed.",
	fiber.StatusConflict,
)

// A key arriving in the path skips the struct tag that validates one in a body,
// so without this a malformed key reached the database and came back as a check
// constraint violation — a 500 for what is the caller's mistake.
var errInvalidTemplateKey = apperrors.New(
	"template.invalid_key",
	"A template key is 3–64 characters of lowercase letters, digits, dots and hyphens, starting and ending with a letter or digit.",
	fiber.StatusBadRequest,
)

type TemplateHandler interface {
	CreateTemplate(c fiber.Ctx) error
	GetTemplates(c fiber.Ctx) error
	GetTemplate(c fiber.Ctx) error
	UpdateTemplate(c fiber.Ctx) error
	DeleteTemplate(c fiber.Ctx) error
	RestoreTemplate(c fiber.Ctx) error
	GetTemplateByKey(c fiber.Ctx) error
	UpsertTemplateByKey(c fiber.Ctx) error
	ListTemplateVersions(c fiber.Ctx) error
	GetTemplateVersion(c fiber.Ctx) error
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
//	@Description	Create a new email template with the provided name, HTML content, and plain text content. Records the content as the template's first version: an operator's, published at once.
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

	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindOperator), func(q *database.Queries) (database.Template, error) {
		return q.CreateTemplate(c.Context(), database.CreateTemplateParams{
			Name:              params.Name,
			Subject:           params.Subject,
			HtmlContent:       params.HTMLContent,
			PlainTextContent:  params.PlainTextContent,
			ReactEmailContent: params.ReactEmailContent,
			Key:               params.Key,
		})
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
//	@Param			_start		query		int		false	"Start index"
//	@Param			_end		query		int		false	"End index"
//	@Param			lifecycle	query		string	false	"Lifecycle filter: current, inactive, all"	Enums(current,inactive,all)	default(current)
//	@Success		200			{array}		database.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates [get]
func (h *templateHandlerImpl) GetTemplates(c fiber.Ctx) error {
	limit, offset := getPaginationParams(c)

	lifecycle, err := parseLifecycleFilter(c.Query("lifecycle"))
	if err != nil {
		return err
	}

	var templates []database.Template
	var count int64
	switch lifecycle {
	case lifecycleCurrent:
		templates, err = h.db.GetAllTemplates(c.Context(), database.GetAllTemplatesParams{Limit: limit, Offset: offset})
		if err == nil {
			count, err = h.db.CountTemplates(c.Context())
		}
	case lifecycleInactive:
		templates, err = h.db.GetArchivedTemplates(c.Context(), database.GetArchivedTemplatesParams{Limit: limit, Offset: offset})
		if err == nil {
			count, err = h.db.CountArchivedTemplates(c.Context())
		}
	case lifecycleAll:
		templates, err = h.db.GetAllTemplatesIncludingArchived(c.Context(), database.GetAllTemplatesIncludingArchivedParams{Limit: limit, Offset: offset})
		if err == nil {
			count, err = h.db.CountAllTemplatesIncludingArchived(c.Context())
		}
	}
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
//	@Description	Update an existing email template with the provided ID and details. Records the content the template ends up with as an operator's version, published at once.
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

	existing, err := h.db.GetTemplateById(c.Context(), id)
	if err != nil {
		return err
	}

	key := params.Key
	if existing.System {
		// The content of a system template may be reworded freely; its key is the
		// contract another service calls it by, so that stays put.
		if !sameKey(key, existing.Key) {
			return errSystemTemplateKey
		}
		key = existing.Key
	}

	// The old panel has no drafts: its edit is an operator's version, published
	// at once, as its saves always went straight to live mail.
	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindOperator), func(q *database.Queries) (database.Template, error) {
		return q.UpdateTemplate(c.Context(), database.UpdateTemplateParams{
			ID:                id,
			Name:              params.Name,
			Subject:           params.Subject,
			HtmlContent:       params.HTMLContent,
			PlainTextContent:  params.PlainTextContent,
			ReactEmailContent: params.ReactEmailContent,
			Key:               key,
		})
	})
	if err != nil {
		return err
	}

	return c.JSON(template)
}

// DeleteTemplate godoc
//
//	@Summary		Archive an email template
//	@Description	Archive an email template without removing historical mail tasks or queue items. Repeating the request is safe.
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

	existing, err := h.db.GetTemplateByIdIncludingArchived(c.Context(), id)
	if err != nil {
		return err
	}
	if existing.System {
		return errSystemTemplateArchive
	}

	if _, err := h.db.ArchiveTemplate(c.Context(), database.ArchiveTemplateParams{
		ID: id, ArchivedBy: lifecycleActor(c.Locals("user_id")),
	}); err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// RestoreTemplate godoc
//
//	@Summary		Restore an archived email template
//	@Description	Restore an archived email template. Repeating the request is safe.
//	@Tags			Templates
//	@Produce		json
//	@Param			id	path		string	true	"Template ID"
//	@Success		200	{object}	database.Template
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		409	{object}	apperrors.AppError	"Conflict"
//	@Router			/templates/{id}/restore [post]
func (h *templateHandlerImpl) RestoreTemplate(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	template, err := h.db.RestoreTemplate(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(template)
}

// versionAuthor is who a request writes a Mail template version as: the
// subject and name of its token, under the kind of writer the route serves.
func versionAuthor(c fiber.Ctx, kind database.TemplateAuthorKind) database.VersionAuthor {
	return database.VersionAuthor{
		Kind: kind,
		Sub:  lifecycleActor(c.Locals("user_id")),
		Name: lifecycleActor(c.Locals("user_name")),
	}
}

func sameKey(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// GetTemplateByKey godoc
//
//	@Summary		Get an email template by key
//	@Description	Get a template by its stable key (for example keycloak.verify-email).
//	@Tags			Templates
//	@Produce		json
//	@Param			key	path		string	true	"Template key"
//	@Success		200	{object}	database.Template
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Router			/templates/by-key/{key} [get]
func (h *templateHandlerImpl) GetTemplateByKey(c fiber.Ctx) error {
	key := c.Params("key")
	if key == "" {
		return apperrors.ErrStatusNotFound
	}

	template, err := h.db.GetTemplateByKey(c.Context(), &key)
	if err != nil {
		return err
	}

	return c.JSON(template)
}

// UpsertTemplateByKey godoc
//
//	@Summary		Create or replace a template addressed by key
//	@Description	Seed path for system templates: creates the template when the key is new and replaces its content when it already exists. Un-archives the template so a seed always leaves a usable template behind. Records the content the template ends up with as a Template seed version, published at once.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			key			path		string							true	"Template key"
//	@Param			template	body		requests.UpsertTemplateByKey	true	"Template details"
//	@Success		200			{object}	database.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/by-key/{key} [put]
func (h *templateHandlerImpl) UpsertTemplateByKey(c fiber.Ctx) error {
	key := c.Params("key")
	if key == "" {
		return apperrors.ErrStatusNotFound
	}
	if !validator.IsTemplateKey(key) {
		return errInvalidTemplateKey
	}

	var params requests.UpsertTemplateByKey
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	// A Template seed's version is published at once. It is taken from the row
	// the upsert leaves, so a subject the upsert kept is the subject recorded.
	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindTemplateSeed), func(q *database.Queries) (database.Template, error) {
		return q.UpsertTemplateByKey(c.Context(), database.UpsertTemplateByKeyParams{
			Key:               key,
			Name:              params.Name,
			Subject:           params.Subject,
			HtmlContent:       params.HTMLContent,
			PlainTextContent:  params.PlainTextContent,
			ReactEmailContent: params.ReactEmailContent,
			System:            params.System,
		})
	})
	if err != nil {
		return err
	}

	return c.JSON(template)
}
