package requests

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
