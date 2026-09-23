package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/requiredvars"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
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

var errDraftDiscarded = apperrors.New(
	"template.draft_discarded",
	"This draft was discarded, so it is not published. To use it again, restore it as a new draft (POST /v1/templates/{id}/versions/{versionId}/restore).",
	fiber.StatusConflict,
)

var errNotADraft = apperrors.New(
	"template.not_a_draft",
	"Only a draft is published. To publish this version again, restore it as a new draft (POST /v1/templates/{id}/versions/{versionId}/restore).",
	fiber.StatusConflict,
)

// errStaleBase refuses to publish a draft someone else's publish overtook. Its
// params name the three versions involved, so the editor can show the draft
// beside what is published now and let the operator choose.
var errStaleBase = apperrors.New(
	"template.stale_base",
	"A newer version was published after this draft was started. Compare the two; to replace it, publish again with force naming it: {\"force\": {\"over_version_id\": params.published_version_id}}.",
	fiber.StatusConflict,
)

// SaveTemplateDraft godoc
//
//	@Summary		Save a draft of a template
//	@Description	Records an operator's draft of a Mail template: a version that is sent to nobody until it is published. The template row, which is what is sent, does not change. Changing which source is the Main source is a save too: send the new main_mode and the render its source gives.
//	@Description
//	@Description	The editor renders the Main source; the server stores the render it is given. It checks what it can without rendering: the fields are there and not blank, the Main source's Authoring mode holds a source, the base is a published version of this template, a Visual source is a JSON object, a JSX source has code in it, and — as every write of a version is checked, by requiredvars — the subject, plain text and HTML parse as the mailer's Go templates (422 template.unparseable, params.part naming subject, plain_text or html) and the HTML references every Required variable of the template as it stands now (422 template.required_variables_missing, params.missing: [{name, source, reason}]). An archived template is not found.
//	@Description
//	@Description	Each save is a new version; an operator's newest version, while unpublished, is their draft in progress. A save continues it when it started from the same base, or else starts from the base. Sources left out, or null, are kept from the version the save continues, so a save never drops a source; so is the name, and a name sent renames the template when the draft is published. A save that changes nothing records nothing and answers 200 with the version it continues.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"Template ID"
//	@Param			draft	body		requests.SaveTemplateDraft	true	"The draft"
//	@Success		201		{object}	handlers.TemplateVersion	"The draft, written"
//	@Success		200		{object}	handlers.TemplateVersion	"Nothing changed: the version the save would have repeated"
//	@Failure		400		{object}	apperrors.AppError			"validation.error (params.errors, sorted by field), template.invalid_base or template.main_source_missing"
//	@Failure		403		{object}	apperrors.AppError			"Forbidden"
//	@Failure		404		{object}	apperrors.AppError			"Not Found: no such template, or it is archived"
//	@Failure		422		{object}	apperrors.AppError			"template.unparseable: params.part (subject, plain_text or html) does not parse, params.error is the parser's message; or template.required_variables_missing: params.missing names each Required variable the HTML drops, with its source (contract or operator) and reason"
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
		Name:             nameOrKept(params.Name),
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
//	@Description	Makes a draft the version the template sends: the draft is marked published and copied onto the template row — name, subject, HTML and plain text, and react_email_content: the JSX source when JSX is the Main source, an empty string otherwise, so the old panel never re-renders a JSX source that is not what is sent — in one transaction. Answers with the template as publishing left it. Publishing the version the template already sends changes nothing and answers the same way. The draft is checked again as it was when saved, against the template's Required variables as they stand at publishing.
//	@Description
//	@Description	A draft is stale when its base_version_id is not the template's published_version_id: someone published after it was started, and publishing it would quietly revert their version. That is refused with 409 template.stale_base, whose params name version_id (the draft), base_version_id (what it started from) and published_version_id (what is sent now), so both can be shown side by side. The operator's confirmation names the version they saw: {"force": {"over_version_id": <published_version_id from the conflict>}} publishes the draft over it, and the replaced version stays in the history. If another version was published since, the confirmation is refused with a fresh 409 naming it.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string							true	"Template ID"
//	@Param			versionId	path		string							true	"Version ID of the draft"
//	@Param			publish		body		requests.PublishTemplateVersion	false	"The version a stale draft is published over"
//	@Success		200			{object}	handlers.Template
//	@Failure		400			{object}	apperrors.AppError	"validation.error"
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"Not Found: no such template or version, or the template is archived"
//	@Failure		409			{object}	apperrors.AppError	"template.stale_base, template.not_a_draft or template.draft_discarded"
//	@Failure		422			{object}	apperrors.AppError	"template.unparseable (params.part: subject, plain_text or html), or template.required_variables_missing: the draft drops a Required variable the template has now, a variable marked since it was saved included (params.missing: [{name, source, reason}])"
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

	var over *uuid.UUID
	if params.Force != nil {
		if params.Force.OverVersionID == uuid.Nil {
			return apperrors.ErrValidation.WithParams(map[string]interface{}{
				"errors": []validator.FieldError{{Field: "over_version_id", Code: "required"}},
			})
		}
		over = &params.Force.OverVersionID
	}

	template, err := h.db.PublishTemplateDraft(c.Context(), templateID, versionID, over, checkVersion)
	if err != nil {
		return draftError(err)
	}
	return h.sendTemplate(c, fiber.StatusOK, template)
}

// RestoreTemplateVersion godoc
//
//	@Summary		Restore a version as a draft
//	@Description	Copies any version of the template — its subject, sources, Main source and render — into a new draft by the caller, started from the version published now. Nothing that is sent changes; the draft is published like any other. Another operator's draft can be restored too. When the copy would repeat the version it continues — the caller's draft in progress on the published version, or else the published version — nothing is recorded and that version is answered with 200.
//	@Tags			Templates
//	@Produce		json
//	@Param			id			path		string						true	"Template ID"
//	@Param			versionId	path		string						true	"Version ID"
//	@Success		201			{object}	handlers.TemplateVersion	"The draft, written"
//	@Success		200			{object}	handlers.TemplateVersion	"Nothing changed: the version the copy would have repeated"
//	@Failure		403			{object}	apperrors.AppError			"Forbidden"
//	@Failure		404			{object}	apperrors.AppError			"Not Found: no such template or version, or the template is archived"
//	@Failure		422			{object}	apperrors.AppError			"template.unparseable: the copy would not parse; or template.required_variables_missing: it drops a Required variable the template has now (params.missing: [{name, source, reason}]) — checked as a saved draft is"
//	@Failure		500			{object}	apperrors.AppError			"Internal Server Error"
//	@Router			/templates/{id}/versions/{versionId}/restore [post]
func (h *templateHandlerImpl) RestoreTemplateVersion(c fiber.Ctx) error {
	templateID, versionID, err := versionPath(c)
	if err != nil {
		return err
	}

	draft, created, err := h.db.RestoreTemplateVersion(c.Context(), templateID, versionID, versionAuthor(c, database.TemplateAuthorKindOperator), checkVersion)
	if err != nil {
		return draftError(err)
	}
	return sendDraft(c, draft, created)
}

// checkVersion is the one place for rules on what a version's content says,
// beyond what the store keeps whole: it runs on a draft before it is saved —
// a restored copy included — and again before it is published, inside the
// write's transaction with the template row locked, and whatever it returns
// refuses the write. template is that locked row, so the Required variables
// are the template's as they stand at that moment; content is the version's
// own, not the row's.
//
// The rules are requiredvars', the same the old panel's and the seed's
// writes obey: the mailer parses the subject, plain text and HTML, or every
// send of the version fails (template.unparseable, params.part), and the HTML
// references every Required variable (template.required_variables_missing).
func checkVersion(_ context.Context, _ *database.Queries, template database.Template, content database.VersionContent) error {
	if err := requiredvars.CheckParts(content.Subject, content.PlainTextContent, content.HTMLContent); err != nil {
		return err
	}
	return requiredvars.CheckBody(template, content.HTMLContent)
}

// nameOrKept is a draft's name as the store takes it: empty when the save
// leaves the name as the version it continues has it.
func nameOrKept(name *string) string {
	if name == nil {
		return ""
	}
	return *name
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
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		problems = append(problems, validator.FieldError{Field: "name", Code: "required"})
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
	sort.Slice(problems, func(i, j int) bool { return problems[i].Field < problems[j].Field })
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
	case errors.Is(err, database.ErrInvalidBase):
		return errInvalidBase
	case errors.Is(err, database.ErrNotADraft):
		return errNotADraft
	case errors.Is(err, database.ErrDraftDiscarded):
		return errDraftDiscarded
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

// DiscardTemplateVersion godoc
//
//	@Summary		Discard a draft
//	@Description	Gives up a draft: it stays in the history, readable and restorable, but it is nobody's draft in progress any more — it leaves the template's drafts, and the next save starts from the published version — and it is never published. Any operator with the write role can discard any draft. Discarding a discarded draft changes nothing and answers the same way. Answers with the version as discarding left it.
//	@Tags			Templates
//	@Produce		json
//	@Param			id			path		string	true	"Template ID"
//	@Param			versionId	path		string	true	"Version ID of the draft"
//	@Success		200			{object}	handlers.TemplateVersion
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"Not Found: no such template or version, or the template is archived"
//	@Failure		409			{object}	apperrors.AppError	"template.not_a_draft: the version is published"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/versions/{versionId}/discard [post]
func (h *templateHandlerImpl) DiscardTemplateVersion(c fiber.Ctx) error {
	templateID, versionID, err := versionPath(c)
	if err != nil {
		return err
	}
	discarded, err := h.db.DiscardTemplateDraft(c.Context(), templateID, versionID)
	if err != nil {
		return draftError(err)
	}
	return c.JSON(templateVersion(discarded))
}
