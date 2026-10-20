package requests

import "github.com/google/uuid"

// MaxNotificationRecipients is how many people one POST /v1/notifications
// may name.
const MaxNotificationRecipients = 1000

// SendNotification is POST /v1/notifications: what happened (type), to whom
// (recipients, people by their Keycloak sub), the variables its text needs
// and where it came from. SkyMail picks each person's channels.
type SendNotification struct {
	// The notification type's key (GET /notification_types), the Template key's alphabet.
	Type string `json:"type" validate:"required,templatekey" example:"event.cancelled"`
	// The people, each once (a sub named again is counted in duplicates and left out).
	Recipients []NotificationRecipient `json:"recipients" validate:"required,min=1,max=1000,dive"`
	// The variables the type's texts read, the same for every recipient; each recipient's own name and address are .FullName and .Email.
	Variables map[string]interface{} `json:"variables"`
	// Where the notification came from, kept with it.
	Source *NotificationSource `json:"source"`
}

// NotificationRecipient is one person a notification goes to.
type NotificationRecipient struct {
	// The person's Keycloak subject.
	Sub uuid.UUID `json:"sub" validate:"required" example:"31ef736f-72da-4a40-8791-d523199cf9f0"`
	// The address to e-mail, when the caller knows it; left out, SkyMail reads the person's primary e-mail from Keycloak.
	Email string `json:"email" validate:"omitempty,email,max=254" example:"ayse@example.com"`
	// The name the mail greets; left out, the name in Keycloak.
	Name string `json:"name" validate:"max=200" example:"Ayşe Kaya"`
	// The channels the person's preferences allow for this notification, as core resolved them. Required for an optional type (a person with [] gets nothing); a required type ignores it.
	AllowedChannels *[]string `json:"allowed_channels" validate:"omitempty,dive,oneof=email inbox push sms" example:"email"`
}

// NotificationSource is where a notification came from.
type NotificationSource struct {
	Kind string `json:"kind" validate:"required,max=64" example:"event"`
	Ref  string `json:"ref" validate:"max=200" example:"6f1c2d3e-0000-4000-8000-000000000000"`
}

// PutNotificationType is the Template seed's statement of a notification type.
type PutNotificationType struct {
	// The preference category the type belongs to; null when none.
	Category *string `json:"category" validate:"omitempty,max=64" example:"events"`
	// A required type does not look at preferences (a login code, a ticket, a certificate).
	Required bool `json:"required"`
	// The channels the type goes by. Today only email.
	DefaultChannels []string `json:"default_channels" validate:"required,min=1,dive,oneof=email" example:"email"`
	// The queue lane: system (never waits behind a bulk send) or bulk.
	Lane string `json:"lane" validate:"required,oneof=system bulk" example:"bulk"`
}
