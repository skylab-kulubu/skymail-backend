package requests

import "encoding/json"

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
