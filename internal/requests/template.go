package requests

type CreateTemplate struct {
	Name              string `json:"name" validate:"required"`
	Subject           string `json:"subject" validate:"required"`
	HTMLContent       string `json:"html_content" validate:"required"`
	PlainTextContent  string `json:"plain_text_content" validate:"required"`
	ReactEmailContent string `json:"react_email_content" validate:"required"`
}

type UpdateTemplate struct {
	Name              string `json:"name" validate:"required"`
	Subject           string `json:"subject" validate:"required"`
	HTMLContent       string `json:"html_content" validate:"required"`
	PlainTextContent  string `json:"plain_text_content" validate:"required"`
	ReactEmailContent string `json:"react_email_content" validate:"required"`
}
