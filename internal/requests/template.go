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
	Name             string `json:"name" validate:"required"`
	Subject          string `json:"subject" validate:"required"`
	HTMLContent      string `json:"html_content" validate:"required"`
	PlainTextContent string `json:"plain_text_content" validate:"required"`
	// The template's JSX source: its .tsx file in the repo as it is, of which html_content and plain_text_content are the render. It becomes the version's JSX Main source, which the panel's JSX mode opens. Text with nothing but comments in it — the pointer comment the seed sent before it sent the source — is no source: html_content is then the HTML Main source.
	ReactEmailContent string `json:"react_email_content" validate:"required"`
	System            bool   `json:"system"`
	// The Required variables the sending service's contract declares, which the body must keep referencing, each with why the mail needs it. An entry may also be the name alone, as a string; its reason is then null. They replace the template's contract set; a name among them leaves the operators' set. Leave the field out (or null) to keep the set the template has; send [] to clear it.
	ContractRequiredVariables *[]ContractVariable `json:"contract_required_variables" validate:"omitempty,max=50,dive"`
}

// ContractVariable is one contract Required variable the Template seed sends:
// {"name", "reason"}, or the name alone as a string — what a seed from before
// reasons sends.
type ContractVariable struct {
	// The variable, as the body reaches it with .Name.
	Name string `json:"name" validate:"required,variablename" example:"link"`
	// Why the mail cannot do without it, in a sentence an operator reads.
	Reason *string `json:"reason" validate:"omitempty,max=300" example:"Parola sıfırlama bağlantısı; kaldırılırsa mail işe yaramaz."`
}

// UnmarshalJSON takes an entry as an object or as the name alone.
func (v *ContractVariable) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*v = ContractVariable{Name: name}
		return nil
	}
	type entry ContractVariable
	return json.Unmarshal(data, (*entry)(v))
}

// AddRequiredVariable is a variable an operator marks required.
type AddRequiredVariable struct {
	// The variable, as the body reaches it with .Name.
	Name string `json:"name" validate:"required,variablename" example:"EventUrl"`
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
	// Publish a stale draft anyway, replacing the version named. Left out: a stale draft is refused.
	Force *PublishOver `json:"force"`
}

// PublishOver is an operator's confirmation that a stale draft replaces the
// version they were shown.
type PublishOver struct {
	// The published_version_id of the template.stale_base conflict: the version the operator saw and chose to replace. If another has been published since, the publish is refused again, naming it. The replaced version stays in the history.
	OverVersionID uuid.UUID `json:"over_version_id" validate:"required"`
}
