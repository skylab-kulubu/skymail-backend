package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

var errTemplateArchived = apperrors.New(
	"template.archived",
	"An archived template takes no drafts and is not published; restore it first.",
	fiber.StatusConflict,
)

var errInvalidBase = apperrors.New(
	"template.invalid_base",
	"base_version_id must be a published version of this template: the one the draft started from.",
	fiber.StatusBadRequest,
)

var errMainSourceMissing = apperrors.New(
	"template.main_source_missing",
	"The Main source's Authoring mode holds no source.",
	fiber.StatusBadRequest,
)

var errInvalidBody = apperrors.New(
	"template.invalid_body",
	"The subject, plain text or HTML does not parse as a Go template, so no send of it could go out.",
	fiber.StatusBadRequest,
)

var errNotADraft = apperrors.New(
	"template.not_a_draft",
	"Only a draft is published. Restore this version as a draft to publish it again.",
	fiber.StatusConflict,
)

// errStaleBase refuses to publish a draft someone else's publish overtook. Its
// params name the three versions involved, so the editor can show the draft
// beside what is published now and let the operator choose.
var errStaleBase = apperrors.New(
	"template.stale_base",
	"A newer version was published after this draft was started. Compare the two; publish again with force to replace it.",
	fiber.StatusConflict,
)

// SaveTemplateDraft godoc
//
//	@Summary		Save a draft of a template
//	@Description	Records an operator's draft of a Mail template: a version that is sent to nobody until it is published. The template row, which is what is sent, does not change. Changing which source is the Main source is a save too: send the new main_mode and the render its source gives.
//	@Description
//	@Description	The editor renders the Main source; the server stores the render it is given. It checks what it can without rendering: the fields are there and not blank, the Main source's Authoring mode holds a source, the base is a published version of this template, a Visual source is a JSON object, a JSX source has code in it, and the subject, plain text and HTML parse as the mailer's Go templates.
//	@Description
//	@Description	Each save is a new version; an operator's newest version, while unpublished, is their draft in progress. A save continues it when it started from the same base, or else starts from the base. Sources left out, or null, are kept from the version the save continues, so a save never drops a source. A save that changes nothing records nothing and answers 200 with the version it would have repeated: the draft in progress, or with none the base.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Template ID"
//	@Param			draft	body		requests.SaveTemplateDraft	true	"The draft"
//	@Success		201		{object}	handlers.TemplateVersion	"The draft, written"
//	@Success		200		{object}	handlers.TemplateVersion	"Nothing changed: the version the save would have repeated"
//	@Failure		400		{object}	apperrors.AppError			"validation.error, template.invalid_base, template.main_source_missing or template.invalid_body (params.field names the part)"
//	@Failure		403		{object}	apperrors.AppError			"Forbidden"
//	@Failure		404		{object}	apperrors.AppError			"Not Found"
//	@Failure		409		{object}	apperrors.AppError			"template.archived"
//	@Failure		500		{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/templates/{id}/drafts [post]
func (h *templateHandlerImpl) SaveTemplateDraft(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	var params requests.SaveTemplateDraft
	if err := bindBody(c, &params); err != nil {
		return err
	}
	if problems := draftProblems(params); len(problems) > 0 {
		return apperrors.ErrValidation.WithParams(map[string]interface{}{"errors": problems})
	}

	draft, created, err := h.db.SaveTemplateDraft(c.Context(), id, versionAuthor(c, database.TemplateAuthorKindOperator), params.BaseVersionID, database.VersionContent{
		Subject:          params.Subject,
		JSXSource:        params.JSXSource,
		VisualSource:     jsonValue(params.VisualSource),
		HTMLSource:       params.HTMLSource,
		MainMode:         database.AuthoringMode(params.MainMode),
		HTMLContent:      params.HTMLContent,
		PlainTextContent: params.PlainTextContent,
	}, checkVersion)
	if err != nil {
		return draftError(err)
	}
	return sendDraft(c, draft, created)
}

// PublishTemplateVersion godoc
//
//	@Summary		Publish a draft
//	@Description	Makes a draft the version the template sends: the draft is marked published and copied onto the template row — subject, HTML, plain text, and its JSX source (or an empty string) as react_email_content — in one transaction. Answers with the template as publishing left it. Publishing the version the template already sends changes nothing and answers the same way.
//	@Description
//	@Description	A draft is stale when its base_version_id is not the template's published_version_id: someone published after it was started, and publishing it would quietly revert their version. That is refused with 409 template.stale_base, whose params name version_id (the draft), base_version_id (what it started from) and published_version_id (what is sent now), so both can be shown side by side. {"force": true} publishes it anyway; the replaced version stays in the history.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string							true	"Template ID"
//	@Param			versionId	path		string							true	"Version ID of the draft"
//	@Param			publish		body		requests.PublishTemplateVersion	false	"Whether to publish over a newer version"
//	@Success		200			{object}	handlers.Template
//	@Failure		400			{object}	apperrors.AppError	"validation.error or template.invalid_body"
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"Not Found"
//	@Failure		409			{object}	apperrors.AppError	"template.stale_base, template.not_a_draft or template.archived"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/versions/{versionId}/publish [post]
func (h *templateHandlerImpl) PublishTemplateVersion(c fiber.Ctx) error {
	templateID, versionID, err := versionPath(c)
	if err != nil {
		return err
	}
	var params requests.PublishTemplateVersion
	if len(bytes.TrimSpace(c.Body())) > 0 {
		if err := bindBody(c, &params); err != nil {
			return err
		}
	}

	template, err := h.db.PublishTemplateDraft(c.Context(), templateID, versionID, params.Force, checkVersion)
	if err != nil {
		return draftError(err)
	}
	return h.sendTemplate(c, fiber.StatusOK, template)
}

// RestoreTemplateVersion godoc
//
//	@Summary		Restore a version as a draft
//	@Description	Copies any version of the template — its subject, sources, Main source and render — into a new draft by the caller, started from the version published now. Nothing that is sent changes; the draft is published like any other. Another operator's draft can be restored too. When the copy would repeat the caller's draft in progress, or with none the published version, nothing is recorded and that version is answered with 200.
//	@Tags			Templates
//	@Produce		json
//	@Param			id			path		string						true	"Template ID"
//	@Param			versionId	path		string						true	"Version ID"
//	@Success		201			{object}	handlers.TemplateVersion	"The draft, written"
//	@Success		200			{object}	handlers.TemplateVersion	"Nothing changed: the version the copy would have repeated"
//	@Failure		403			{object}	apperrors.AppError			"Forbidden"
//	@Failure		404			{object}	apperrors.AppError			"Not Found"
//	@Failure		409			{object}	apperrors.AppError			"template.archived"
//	@Failure		500			{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/templates/{id}/versions/{versionId}/restore [post]
func (h *templateHandlerImpl) RestoreTemplateVersion(c fiber.Ctx) error {
	templateID, versionID, err := versionPath(c)
	if err != nil {
		return err
	}

	draft, created, err := h.db.RestoreTemplateVersion(c.Context(), templateID, versionID, versionAuthor(c, database.TemplateAuthorKindOperator))
	if err != nil {
		return draftError(err)
	}
	return sendDraft(c, draft, created)
}

// checkVersion is the one place for rules on what a version's content says,
// beyond what the store keeps whole: it runs on a draft before it is saved and
// again before it is published, inside the write's transaction with the
// template row locked, and whatever it returns refuses the write. The
// Required variable check belongs here.
//
// The mailer has to be able to parse the subject, plain text and HTML, or
// every send of the version fails.
func checkVersion(_ context.Context, _ *database.Queries, _ database.Template, content database.VersionContent) error {
	err := mailer.CheckTemplate(content.Subject, content.PlainTextContent, content.HTMLContent)
	var unparsable *mailer.ParseError
	if !errors.As(err, &unparsable) {
		return err
	}
	return errInvalidBody.WithParams(map[string]interface{}{
		"field": mailPartFields[unparsable.Part],
		"error": unparsable.Err.Error(),
	})
}

// mailPartFields is the request field each part the mailer parses comes from.
var mailPartFields = map[mailer.MailPart]string{
	mailer.PartSubject:   "subject",
	mailer.PartPlainText: "plain_text_content",
	mailer.PartHTML:      "html_content",
}

// draftProblems are what the struct tags on a draft cannot see: text that is
// only whitespace, and a Visual source that is not a JSON object.
func draftProblems(p requests.SaveTemplateDraft) []validator.FieldError {
	var problems []validator.FieldError
	for field, value := range map[string]string{
		"subject": p.Subject, "html_content": p.HTMLContent, "plain_text_content": p.PlainTextContent,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, validator.FieldError{Field: field, Code: "required"})
		}
	}
	if p.HTMLSource != nil && strings.TrimSpace(*p.HTMLSource) == "" {
		problems = append(problems, validator.FieldError{Field: "html_source", Code: "invalid"})
	}
	if visual := jsonValue(p.VisualSource); visual != nil {
		var document map[string]any
		if json.Unmarshal(visual, &document) != nil || document == nil {
			problems = append(problems, validator.FieldError{Field: "visual_source", Code: "invalid"})
		}
	}
	return problems
}

// bindBody reads a JSON request body. A body that is not the documented shape
// — not JSON, or a field of the wrong type — is the caller's mistake, answered
// with 400 like any other invalid field.
func bindBody(c fiber.Ctx, out any) error {
	err := c.Bind().Body(out)
	var malformed *fiber.BindError
	if errors.As(err, &malformed) {
		field := malformed.Field
		if field == "" {
			field = "body"
		}
		return apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: field, Code: "invalid"}},
		})
	}
	return err
}

// draftError is the API error for what refused a draft, a restore or a
// publish, or err itself when it is none of those.
func draftError(err error) error {
	var stale *database.StaleBaseError
	var mainMissing *database.MainSourceMissingError
	switch {
	case errors.As(err, &stale):
		return errStaleBase.WithParams(map[string]interface{}{
			"version_id":           stale.VersionID,
			"base_version_id":      stale.BaseVersionID,
			"published_version_id": stale.PublishedVersionID,
		})
	case errors.As(err, &mainMissing):
		return errMainSourceMissing.WithParams(map[string]interface{}{"main_mode": mainMissing.MainMode})
	case errors.Is(err, database.ErrTemplateArchived):
		return errTemplateArchived
	case errors.Is(err, database.ErrInvalidBase):
		return errInvalidBase
	case errors.Is(err, database.ErrNotADraft):
		return errNotADraft
	case errors.Is(err, database.ErrNotJSXSource):
		return apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: "jsx_source", Code: "invalid"}},
		})
	}
	return err
}

// sendDraft answers with the version a draft save or a restore left: 201 when
// it wrote one, 200 when there was nothing to write.
func sendDraft(c fiber.Ctx, version database.GetTemplateVersionRow, created bool) error {
	status := fiber.StatusOK
	if created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(templateVersion(version))
}

// versionPath reads the template and version a version route names. An id
// that is not a UUID names nothing: 404.
func versionPath(c fiber.Ctx) (uuid.UUID, uuid.UUID, error) {
	templateID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, apperrors.ErrStatusNotFound
	}
	versionID, err := uuid.Parse(c.Params("versionId"))
	if err != nil {
		return uuid.Nil, uuid.Nil, apperrors.ErrStatusNotFound
	}
	return templateID, versionID, nil
}

// jsonValue is a JSON field as the database stores it: nil when the field was
// left out or null.
func jsonValue(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return trimmed
}
