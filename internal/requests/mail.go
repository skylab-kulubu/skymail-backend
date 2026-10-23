package requests

import (
	"github.com/google/uuid"
)

// SendPurpose is what a send says about its stream (ADR 0072 decision 2),
// read only while SkyMail's purposes are on (SKYMAIL_MAIL_PURPOSE; GET
// /mail_tasks/tracking-mode purpose_available) and ignored while they are off.
// Leave all four out and the send gets the purpose it has always had: duyuru
// (category club:duyuru) for a list, a Keycloak group, a Forms source and a
// Mail onayı request, and for a single mail duyuru when it is tracked, islem
// when it is not. category and the two statements come only with a purpose.
type SendPurpose struct {
	// islem: about the recipient's own application or transaction, only to people one by one (POST /mail_tasks/single, a Mail onayı request to people) or to a Forms source's respondents, with transactional_statement true, never tracked, never with a category; it goes out from bilgi@ without List-Unsubscribe and skips the unsubscribe record. duyuru: an announcement to an audience, with a category; it goes out from duyuru@ and follows the preference and the unsubscribe record. A list or a Keycloak group is always duyuru.
	Purpose *string `json:"purpose" enums:"islem,duyuru"`
	// core's category of a duyuru send: club:duyuru, club:haber, team:<Keycloak group id>:duyuru or team:<id>:haber. Required with duyuru, refused with islem. Whether the group is a team is core's to say (a later version).
	Category *string `json:"category" example:"club:duyuru"`
	// With islem, required and true: the sender's statement that the mail is about the recipient's own application or transaction (a changed place or time, a missing document, a result). Ignored with duyuru.
	TransactionalStatement *bool `json:"transactional_statement"`
	// With duyuru: the sender's statement that the mail carries no commercial content (no sponsor promotion, paid ticket, company job advert or draw; ADR 0072 decision 9). Kept as given; a later version requires it true. Ignored with islem.
	CommercialContentAbsent *bool `json:"commercial_content_absent"`
}

type CreateMailTask struct {
	TemplateID    uuid.UUID              `json:"template_id" validate:"required"`
	MailListID    uuid.UUID              `json:"mail_list_id" validate:"required"`
	BodyVariables map[string]interface{} `json:"body_variables"`
	// Open and click tracking: each member of a Keycloak group whom core lets be tracked gets a pixel and tracked links. Ignored (and answered false) while SkyMail's tracking is off and for a System or sensitive template, which are never tracked; a mailing list's address-only recipients are not tracked (no_account).
	Tracking bool `json:"tracking"`
	SendPurpose
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
	// Ignored for a service's token: its mail is always islem.
	SendPurpose
	// Open and click tracking for a mail a person sends (the compose form's people). Ignored (and answered false) while SkyMail's tracking is off, and for a System template or a sensitive one (a template that says nothing counts as sensitive for one person). A service's token (account and transaction mail) does not ask: the field is ignored, and its mail is tracked only while SkyMail tracks system mail (SKYMAIL_TRACKING_SYSTEM), in the system lane, with no click address on a link a variable fills and none at all in a sensitive mail. The recipient's account is looked up in Keycloak by address before the send is queued: the one enabled user whose primary e-mail equals it (case-insensitively) and is verified; none such is no_account, Keycloak not answering is preference_unavailable, and core decides for the account found.
	Tracking bool `json:"tracking"`
}

// CreateFormMailSource is accepted only from the configured Forms service.
// Forms derives OwnerSub and every other field after its HTTP caller's form
// response permission check. The browser never creates these rows.
type CreateFormMailSource struct {
	OwnerSub       uuid.UUID               `json:"owner_sub" validate:"required"`
	FormID         uuid.UUID               `json:"form_id" validate:"required"`
	FormTitle      string                  `json:"form_title" validate:"required,max=100"`
	Recipients     []MailApprovalRecipient `json:"recipients" validate:"required,min=1,dive"`
	ResponseCount  int                     `json:"response_count" validate:"min=0"`
	SkippedCount   int                     `json:"skipped_count" validate:"min=0"`
	DuplicateCount int                     `json:"duplicate_count" validate:"min=0"`
}

type SendFormSource struct {
	FormSourceID  uuid.UUID              `json:"form_source_id" validate:"required"`
	TemplateID    uuid.UUID              `json:"template_id" validate:"required"`
	BodyVariables map[string]interface{} `json:"body_variables"`
	Tracking      bool                   `json:"tracking"`
	SendPurpose
}
