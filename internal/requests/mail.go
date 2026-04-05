package requests

import (
	"github.com/google/uuid"
)

type CreateMailTask struct {
	TemplateID    uuid.UUID              `json:"template_id" validate:"required"`
	MailListID    uuid.UUID              `json:"mail_list_id" validate:"required"`
	BodyVariables map[string]interface{} `json:"body_variables"`
}
