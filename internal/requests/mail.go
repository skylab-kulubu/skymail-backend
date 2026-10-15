package requests

import (
	"github.com/google/uuid"
)

type CreateMailTask struct {
	TemplateID    uuid.UUID              `json:"template_id" validate:"required"`
	MailListID    uuid.UUID              `json:"mail_list_id" validate:"required"`
	BodyVariables map[string]interface{} `json:"body_variables"`
	// Open and click tracking: each member of a Keycloak group whom core lets be tracked gets a pixel and tracked links. Ignored (and answered false) while SkyMail's tracking is off and for a System or sensitive template, which are never tracked; a mailing list's address-only recipients are not tracked.
	Tracking bool `json:"tracking"`
}

// SendSingleMail addresses the template either by id or by key. Keycloak's mail
// provider uses the key so a reseed cannot break it; core keeps sending the uuid
// it already has in SKYMAIL_WELCOME_TEMPLATE_ID / SKYMAIL_CERTIFICATE_TEMPLATE_ID.
type SendSingleMail struct {
	TemplateID        uuid.UUID              `json:"template_id"`
	TemplateKey       string                 `json:"template_key" validate:"omitempty,templatekey"`
	RecipientEmail    string                 `json:"recipient_email" validate:"required,email"`
	RecipientFullName string                 `json:"recipient_full_name"`
	BodyVariables     map[string]interface{} `json:"body_variables"`
}
