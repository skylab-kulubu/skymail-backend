package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/ptr"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/requiredvars"
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

// A contract variable is the sending service's, declared in the repo and
// written by the Template seed; like the Template key, the panel cannot
// release it.
var errContractRequiredVariable = apperrors.New(
	"template.required_variable_in_contract",
	"The variable is required by the sending service's contract; only the Template seed changes that set.",
	fiber.StatusConflict,
)

var errInvalidVariableName = apperrors.New(
	"template.invalid_variable_name",
	"A variable name is 1–64 letters, digits and underscores, not starting with a digit.",
	fiber.StatusBadRequest,
)

// Template is a Mail template as the template routes serve it: its row — a
// copy of the published version, which is what is sent — with the Authoring
// mode of that version's Main source and each operator's draft in progress.
type Template struct {
	database.Template
	// The Authoring mode of the Main source the template sends: its published version's. Null for a template with no published version.
	MainMode *string `json:"main_mode" enums:"jsx,visual,html"`
	// Each operator's draft in progress, newest first: the newest version an operator wrote of the template, while it is neither published nor discarded. An operator's earlier drafts are superseded and not listed. A draft whose base_version_id is not published_version_id is stale: a newer version was published after it was started, and publishing it takes force.
	Drafts []TemplateVersionSummary `json:"drafts"`
}

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
	AddRequiredVariable(c fiber.Ctx) error
	RemoveRequiredVariable(c fiber.Ctx) error
	SaveTemplateDraft(c fiber.Ctx) error
	PublishTemplateVersion(c fiber.Ctx) error
	RestoreTemplateVersion(c fiber.Ctx) error
	DiscardTemplateVersion(c fiber.Ctx) error
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
//	@Description	Create a new email template with the provided name, HTML content, and plain text content. Records the content as the template's first version: an operator's, published at once. The subject, plain text and HTML content must parse as Go templates the way the mailer parses them.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			template	body		requests.CreateTemplate	true	"Template details"
//	@Success		201			{object}	handlers.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		422			{object}	apperrors.AppError	"The subject, plain text or HTML content does not parse (template.unparseable, params.part)"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates [post]
func (h *templateHandlerImpl) CreateTemplate(c fiber.Ctx) error {
	var params requests.CreateTemplate

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindOperator), nil, checkedWrite(func(q *database.Queries) (database.Template, error) {
		return q.CreateTemplate(c.Context(), database.CreateTemplateParams{
			Name:              params.Name,
			Subject:           params.Subject,
			HtmlContent:       params.HTMLContent,
			PlainTextContent:  params.PlainTextContent,
			ReactEmailContent: params.ReactEmailContent,
			Key:               params.Key,
		})
	}))
	if err != nil {
		return err
	}

	return h.sendTemplate(c, fiber.StatusCreated, template)
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
//	@Success		200			{array}		handlers.Template
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

	served, err := h.served(c.Context(), templates)
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))

	return c.JSON(served)
}

// GetTemplate godoc
//
//	@Summary		Get an email template by ID
//	@Description	Get details of a specific email template by its ID.
//	@Tags			Templates
//	@Produce		json
//	@Param			id	path		string	true	"Template ID"
//	@Success		200	{object}	handlers.Template
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

	return h.sendTemplate(c, fiber.StatusOK, template)
}

// UpdateTemplate godoc
//
//	@Summary		Update an email template
//	@Description	Update an existing email template with the provided ID and details. Records the content the template ends up with as an operator's version, published at once. The subject, plain text and HTML content must parse as Go templates, and the HTML content must reference every Required variable of the template, or nothing is written.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string					true	"Template ID"
//	@Param			template	body		requests.UpdateTemplate	true	"Template details"
//	@Success		200			{object}	handlers.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		404			{object}	apperrors.AppError	"Not Found"
//	@Failure		422			{object}	apperrors.AppError	"The subject, plain text or HTML content does not parse (template.unparseable, params.part) or the HTML drops a Required variable (template.required_variables_missing, params.missing names each with its set and reason)"
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
		if !ptr.Equal(key, existing.Key) {
			return errSystemTemplateKey
		}
		key = existing.Key
	}

	// The old panel has no drafts: its edit is an operator's version, published
	// at once, as its saves always went straight to live mail.
	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindOperator), nil, checkedWrite(func(q *database.Queries) (database.Template, error) {
		return q.UpdateTemplate(c.Context(), database.UpdateTemplateParams{
			ID:                id,
			Name:              params.Name,
			Subject:           params.Subject,
			HtmlContent:       params.HTMLContent,
			PlainTextContent:  params.PlainTextContent,
			ReactEmailContent: params.ReactEmailContent,
			Key:               key,
		})
	}))
	if err != nil {
		return err
	}

	return h.sendTemplate(c, fiber.StatusOK, template)
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
		ID: id, ArchivedBy: localText(c, "user_id"),
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
//	@Success		200	{object}	handlers.Template
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
	return h.sendTemplate(c, fiber.StatusOK, template)
}

// served is template rows as the template routes serve them.
func (h *templateHandlerImpl) served(ctx context.Context, rows []database.Template) ([]Template, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	modes, err := h.db.ListPublishedMainModes(ctx, ids)
	if err != nil {
		return nil, err
	}
	drafts, err := h.db.ListTemplateDrafts(ctx, ids)
	if err != nil {
		return nil, err
	}

	mainModes := make(map[uuid.UUID]string, len(modes))
	for _, mode := range modes {
		mainModes[mode.TemplateID] = string(mode.MainMode)
	}
	inProgress := make(map[uuid.UUID][]TemplateVersionSummary, len(rows))
	for _, draft := range drafts {
		inProgress[draft.TemplateID] = append(inProgress[draft.TemplateID], versionSummary(draft))
	}
	served := make([]Template, 0, len(rows))
	for _, row := range rows {
		template := Template{Template: row, Drafts: inProgress[row.ID]}
		if mode, ok := mainModes[row.ID]; ok {
			template.MainMode = &mode
		}
		if template.Drafts == nil {
			template.Drafts = []TemplateVersionSummary{}
		}
		served = append(served, template)
	}
	return served, nil
}

// sendTemplate answers with one template as the template routes serve it.
func (h *templateHandlerImpl) sendTemplate(c fiber.Ctx, status int, row database.Template) error {
	served, err := h.served(c.Context(), []database.Template{row})
	if err != nil {
		return err
	}
	return c.Status(status).JSON(served[0])
}

// versionAuthor is who a request writes a Mail template version as: the
// subject and name of its token, under the kind of writer the route serves.
func versionAuthor(c fiber.Ctx, kind database.TemplateAuthorKind) database.VersionAuthor {
	return database.VersionAuthor{
		Kind: kind,
		Sub:  localText(c, "user_id"),
		Name: localText(c, "user_name"),
	}
}

// checkedWrite is a row write that must leave a subject, plain text and body
// the mailer can parse, and a body that references every Required variable —
// what checkVersion asks of a draft and a publish: it checks
// what the write left, inside the write's transaction, so a refused write —
// and the version it would have been — is rolled back with it.
func checkedWrite(write func(*database.Queries) (database.Template, error)) func(*database.Queries) (database.Template, error) {
	return func(q *database.Queries) (database.Template, error) {
		written, err := write(q)
		if err != nil {
			return written, err
		}
		if err := requiredvars.CheckParts(written.Subject, written.PlainTextContent, written.HtmlContent); err != nil {
			return written, err
		}
		return written, requiredvars.CheckBody(written, written.HtmlContent)
	}
}

// contractSet is a contract set as the database keeps one: sorted by name
// byte by byte, each name once (its first entry wins), a blank reason
// unknown. nil, a set not sent, stays nil.
func contractSet(sent *[]requests.ContractVariable) ([]byte, error) {
	if sent == nil {
		return nil, nil
	}
	set := make(database.ContractVariables, 0, len(*sent))
	for _, variable := range *sent {
		if _, taken := set.Find(variable.Name); taken {
			continue
		}
		reason := variable.Reason
		if reason != nil && strings.TrimSpace(*reason) == "" {
			reason = nil
		}
		set = append(set, database.ContractVariable{Name: variable.Name, Reason: reason})
	}
	slices.SortFunc(set, func(a, b database.ContractVariable) int { return strings.Compare(a.Name, b.Name) })
	return json.Marshal(set)
}

// GetTemplateByKey godoc
//
//	@Summary		Get an email template by key
//	@Description	Get a template by its stable key (for example keycloak.verify-email).
//	@Tags			Templates
//	@Produce		json
//	@Param			key	path		string	true	"Template key"
//	@Success		200	{object}	handlers.Template
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

	return h.sendTemplate(c, fiber.StatusOK, template)
}

// UpsertTemplateByKey godoc
//
//	@Summary		Create or replace a template addressed by key
//	@Description	Seed path for system templates: creates the template when the key is new and replaces its content when it already exists. Un-archives the template so a seed always leaves a usable template behind. Records the content the template ends up with as a Template seed version, published at once. Writes the contract Required variables when sent, and keeps them when not. The subject, plain text and HTML content must parse as Go templates, and the HTML content must reference every Required variable — the contract set it ends up with and the operators' — or nothing is written.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			key			path		string							true	"Template key"
//	@Param			template	body		requests.UpsertTemplateByKey	true	"Template details"
//	@Success		200			{object}	handlers.Template
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		422			{object}	apperrors.AppError	"The subject, plain text or HTML content does not parse (template.unparseable, params.part) or the HTML drops a Required variable (template.required_variables_missing, params.missing names each with its set and reason)"
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

	contract, err := contractSet(params.ContractRequiredVariables)
	if err != nil {
		return err
	}

	// A Template seed's version is published at once. It is taken from the row
	// the upsert leaves, so a subject the upsert kept is the subject recorded.
	template, err := h.db.PublishTemplateWrite(c.Context(), versionAuthor(c, database.TemplateAuthorKindTemplateSeed), &params.Subject, checkedWrite(func(q *database.Queries) (database.Template, error) {
		return q.UpsertTemplateByKey(c.Context(), database.UpsertTemplateByKeyParams{
			Key:                       key,
			Name:                      params.Name,
			Subject:                   params.Subject,
			HtmlContent:               params.HTMLContent,
			PlainTextContent:          params.PlainTextContent,
			ReactEmailContent:         params.ReactEmailContent,
			System:                    params.System,
			ContractRequiredVariables: contract,
		})
	}))
	if err != nil {
		return err
	}

	return h.sendTemplate(c, fiber.StatusOK, template)
}
