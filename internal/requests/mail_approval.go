package requests

import "github.com/google/uuid"

// SubmitMailApproval is a send submitted for Mail onayı, filled in the way a
// send is: a template, and either a mailing list — an internal list or a
// Keycloak group, as POST /mail_tasks takes it — or one recipient, as POST
// /mail_tasks/single takes them; never both.
type SubmitMailApproval struct {
	TemplateID uuid.UUID `json:"template_id" validate:"required"`
	// An internal mailing list or a Keycloak group. Leave it out to send to one recipient.
	MailListID *uuid.UUID `json:"mail_list_id"`
	// The one recipient. Leave it out to send to a mailing list.
	RecipientEmail    string                 `json:"recipient_email" validate:"omitempty,email"`
	RecipientFullName string                 `json:"recipient_full_name"`
	BodyVariables     map[string]interface{} `json:"body_variables"`
}

// ApproveMailApproval approves a pending request and sends it. With no
// body_variables it goes out exactly as submitted; with them, the approver's
// edit goes out instead, recorded and told to the submitter.
type ApproveMailApproval struct {
	// The variables as the approver edited them, whole. Leave them out to send the request as it stands.
	BodyVariables map[string]interface{} `json:"body_variables"`
	// A word for the submitter, mailed with the decision.
	Note string `json:"note" validate:"max=2000"`
}

// ReturnMailApproval hands an approver's edit of a pending request back to
// its submitter, who accepts it — and it goes out — or declines it.
type ReturnMailApproval struct {
	// The variables as the approver edited them, whole. They must differ from the request's.
	BodyVariables map[string]interface{} `json:"body_variables" validate:"required"`
	// A word for the submitter, mailed with the decision.
	Note string `json:"note" validate:"max=2000"`
}

// RejectMailApproval refuses a pending request, with the reason the
// submitter is told.
type RejectMailApproval struct {
	Reason string `json:"reason" validate:"required,max=2000"`
}

// DeclineMailApproval is a submitter refusing an approver's returned edit.
type DeclineMailApproval struct {
	// A word for the record: why the edit was not taken.
	Note string `json:"note" validate:"max=2000"`
}
