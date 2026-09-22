package handlers

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// TemplateVersionSummary is one entry of a Mail template's version history,
// without its sources or render.
type TemplateVersionSummary struct {
	ID         uuid.UUID `json:"id"`
	TemplateID uuid.UUID `json:"template_id"`
	// 1, 2, 3… within the template, in the order versions were written.
	Seq     int    `json:"seq"`
	Subject string `json:"subject"`
	// The subject the Template seed sent for its version. It can differ from subject: until the seed's conflict rule, the seed keeps the subject a template already has. Null on an operator's version and on the first versions, made from templates written before versions were kept.
	RequestedSubject *string `json:"requested_subject"`
	// The Authoring mode whose source is the Main source: its render is what the version sends.
	MainMode  string                 `json:"main_mode" enums:"jsx,visual,html"`
	Author    database.VersionAuthor `json:"author"`
	CreatedAt time.Time              `json:"created_at"`
	// When the version was published; null for a draft.
	PublishedAt *time.Time `json:"published_at"`
	// The published version a draft started from; null for a template's first version.
	BaseVersionID *uuid.UUID `json:"base_version_id"`
	// Whether this is the published version the template sends.
	Current bool `json:"current"`
}

// TemplateVersion is one Mail template version whole: at most one source per
// Authoring mode, and the Main source's render.
type TemplateVersion struct {
	TemplateVersionSummary
	// The JSX source, React Email code; null when the version has none.
	JSXSource *string `json:"jsx_source"`
	// The Visual source, the block editor's document; null when the version has none.
	VisualSource json.RawMessage `json:"visual_source" swaggertype:"object"`
	// The HTML source; null when the version has none.
	HTMLSource *string `json:"html_source"`
	// The Main source rendered as HTML, as the browser or the Template seed rendered it.
	HTMLContent string `json:"html_content"`
	// The Main source rendered as plain text.
	PlainTextContent string `json:"plain_text_content"`
}

// versionSummary is a version as the version routes serve it.
func versionSummary(s database.TemplateVersionSummary) TemplateVersionSummary {
	return TemplateVersionSummary{
		ID:               s.ID,
		TemplateID:       s.TemplateID,
		Seq:              s.Seq,
		Subject:          s.Subject,
		RequestedSubject: s.RequestedSubject,
		MainMode:         string(s.MainMode),
		Author:           s.Author(),
		CreatedAt:        s.CreatedAt,
		PublishedAt:      s.PublishedAt,
		BaseVersionID:    s.BaseVersionID,
		Current:          s.IsCurrent,
	}
}

// ListTemplateVersions godoc
//
//	@Summary		List a template's versions
//	@Description	A Mail template's version history, newest first: who wrote each version (an operator, by the name their token carried, or a Template seed), when, whether it is published or a draft (published_at null), which Authoring mode is its Main source, the published version it started from, and which version the template sends (current). Sources and render are left out; read one version for them. An archived template's history stays readable.
//	@Tags			Templates
//	@Produce		json
//	@Param			id		path		string	true	"Template ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		handlers.TemplateVersionSummary
//	@Header			200		{integer}	X-Total-Count		"Number of versions the template has"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/versions [get]
func (h *templateHandlerImpl) ListTemplateVersions(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	if _, err := h.db.GetTemplateByIdIncludingArchived(c.Context(), id); err != nil {
		return err
	}

	limit, offset := getPaginationParams(c)
	rows, err := h.db.ListTemplateVersions(c.Context(), database.ListTemplateVersionsParams{
		TemplateID: id, Limit: limit, Offset: offset,
	})
	if err != nil {
		return err
	}
	count, err := h.db.CountTemplateVersions(c.Context(), id)
	if err != nil {
		return err
	}

	versions := make([]TemplateVersionSummary, 0, len(rows))
	for _, row := range rows {
		versions = append(versions, versionSummary(row))
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(versions)
}

// GetTemplateVersion godoc
//
//	@Summary		Read one version of a template
//	@Description	One Mail template version whole: its sources (at most one per Authoring mode, null where it has none), which is the Main source, and the Main source's render as HTML and plain text. A version of another template is not found.
//	@Tags			Templates
//	@Produce		json
//	@Param			id			path		string	true	"Template ID"
//	@Param			versionId	path		string	true	"Version ID"
//	@Success		200			{object}	handlers.TemplateVersion
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"Not Found"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/versions/{versionId} [get]
func (h *templateHandlerImpl) GetTemplateVersion(c fiber.Ctx) error {
	templateID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	versionID, err := uuid.Parse(c.Params("versionId"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}

	row, err := h.db.GetTemplateVersion(c.Context(), database.GetTemplateVersionParams{
		TemplateID: templateID, ID: versionID,
	})
	if err != nil {
		return err
	}

	return c.JSON(TemplateVersion{
		TemplateVersionSummary: versionSummary(row.TemplateVersionSummary),
		JSXSource:              row.JsxSource,
		VisualSource:           row.VisualSource,
		HTMLSource:             row.HtmlSource,
		HTMLContent:            row.HtmlContent,
		PlainTextContent:       row.PlainTextContent,
	})
}
