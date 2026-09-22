package requests

import (
	"encoding/json"

	"github.com/google/uuid"
)

type CreateTemplate struct {
	Name              string  `json:"name" validate:"required"`
	Subject           string  `json:"subject" validate:"required"`
	HTMLContent       string  `json:"html_content" validate:"required"`
	PlainTextContent  string  `json:"plain_text_content" validate:"required"`
	ReactEmailContent string  `json:"react_email_content" validate:"required"`
	Key               *string `json:"key" validate:"omitempty,templatekey"`
}

type UpdateTemplate struct {
	Name              string  `json:"name" validate:"required"`
	Subject           string  `json:"subject" validate:"required"`
	HTMLContent       string  `json:"html_content" validate:"required"`
	PlainTextContent  string  `json:"plain_text_content" validate:"required"`
	ReactEmailContent string  `json:"react_email_content" validate:"required"`
	Key               *string `json:"key" validate:"omitempty,templatekey"`
}

// UpsertTemplateByKey is the seed payload: the key travels in the path, so the
// body carries only the content and whether this key is a system template.
type UpsertTemplateByKey struct {
	Name              string `json:"name" validate:"required"`
	Subject           string `json:"subject" validate:"required"`
	HTMLContent       string `json:"html_content" validate:"required"`
	PlainTextContent  string `json:"plain_text_content" validate:"required"`
	ReactEmailContent string `json:"react_email_content" validate:"required"`
	System            bool   `json:"system"`
}

// SaveTemplateDraft is an operator's draft of a Mail template, as the editor
// saves it. The editor renders the Main source; the server stores the render it
// is given. A source that is left out, or null, is kept from the version the
// save continues, so a save never drops a source.
type SaveTemplateDraft struct {
	Subject string `json:"subject" validate:"required"`
	// The Authoring mode whose source is the Main source: html_content and plain_text_content are its render.
	MainMode string `json:"main_mode" validate:"required,oneof=jsx visual html" enums:"jsx,visual,html"`
	// The JSX source, React Email code. Left out or null: kept as it is.
	JSXSource *string `json:"jsx_source"`
	// The Visual source, the block editor's document: a JSON object. Left out or null: kept as it is.
	VisualSource json.RawMessage `json:"visual_source" swaggertype:"object"`
	// The HTML source. Left out or null: kept as it is.
	HTMLSource *string `json:"html_source"`
	// The Main source rendered as HTML, with its Go template actions intact for the mailer to fill per send.
	HTMLContent string `json:"html_content" validate:"required"`
	// The Main source rendered as plain text.
	PlainTextContent string `json:"plain_text_content" validate:"required"`
	// The published version the draft started from: the template's published_version_id when the editor opened it. Null only for a template that has no published version.
	BaseVersionID *uuid.UUID `json:"base_version_id"`
}

// PublishTemplateVersion publishes a draft. The body is optional.
type PublishTemplateVersion struct {
	// Publish even though a newer version was published after the draft was started, replacing it. The replaced version stays in the history.
	Force bool `json:"force"`
}
