package requests

import "github.com/google/uuid"

// SubmitMailApproval is a send submitted for Mail onayı, filled in the way a
// send is: a template, and either a mailing list — an internal list or a
// Keycloak group, as POST /mail_tasks takes it — or 1..100 people, each sent
// to on their own as POST /mail_tasks/single sends to one; never both.
type SubmitMailApproval struct {
	TemplateID uuid.UUID `json:"template_id" validate:"required"`
	// An internal mailing list or a Keycloak group. Choose exactly one of this,
	// manual recipients or form_source_id.
	MailListID *uuid.UUID `json:"mail_list_id"`
	// An immutable snapshot prepared by Forms for this caller. Never with a
	// list or browser-supplied recipients. Manual recipients retain max=100.
	FormSourceID *uuid.UUID `json:"form_source_id"`
	// The people, 1..100, each address once (compared case-insensitively); each gets a send of their own. Omit for a mailing list or Forms source.
	Recipients    []MailApprovalRecipient `json:"recipients" validate:"omitempty,max=100,dive"`
	BodyVariables map[string]interface{}  `json:"body_variables"`
	// Open and click tracking for the send once approved (as POST /mail_tasks takes it). Ignored (and kept false) while SkyMail's tracking is off, for a System or sensitive template (to people, a template that says nothing counts as sensitive) and for a Forms source. People named here are looked up in Keycloak by address when the request is sent, as POST /mail_tasks/single does: one enabled account with that verified primary e-mail, or no_account.
	Tracking bool `json:"tracking"`
}

// MailApprovalRecipient is one person a request goes to.
type MailApprovalRecipient struct {
	Email string `json:"email" validate:"required,email"`
	// Empty when the submitter knows only the address.
	FullName string `json:"full_name"`
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
