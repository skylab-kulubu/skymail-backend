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
	// The Required variables the sending service's contract declares, which the body must keep referencing. They replace the template's contract set; a name among them leaves the operators' set. Leave the field out (or null) to keep the set the template has; send [] to clear it.
	ContractRequiredVariables *[]string `json:"contract_required_variables" validate:"omitempty,max=50,dive,variablename" example:"link"`
}

// AddRequiredVariable is a variable an operator marks required.
type AddRequiredVariable struct {
	// The variable, as the body reaches it with .Name.
	Name string `json:"name" validate:"required,variablename" example:"EventUrl"`
}
