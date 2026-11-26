package requests

// MailAttachment is one Mail attachment (ADR 0082 decision 6): a file a
// service hands with one mail, today core's ticket PDF. Only a service's
// mail to one person takes it (POST /mail_tasks/single, POST /notifications
// to one recipient), only the e-mail goes with it, PDF or PNG, at most 2 and
// 512 KB decoded in all. SkyNotify keeps the file only until the mail is sent
// or fails for good.
type MailAttachment struct {
	// The name the recipient sees. SkyNotify reduces it to letters, digits, '.', '-' and '_' (Turkish letters become their Latin ones, a space a '-'), at most 100 characters, and ends it with the type's extension (.pdf, .png). Never a person's name: the file name stays with the send's report.
	Filename string `json:"filename" example:"bilet-gecekodu-2026.pdf"`
	// application/pdf or image/png; the file must begin as that type does.
	ContentType string `json:"content_type" example:"application/pdf" enums:"application/pdf,image/png"`
	// The file, standard base64 (RFC 4648 §4, with padding).
	ContentBase64 string `json:"content_base64" example:"JVBERi0xLjcK"`
}

// NoAttachments is the attachments field of a send that takes none: a list,
// a Keycloak group, a Mail onayı request, a News send. Sent anyway, it is
// refused (400 attachment.not_allowed): a bulk send carries no person's
// file.
type NoAttachments struct {
	Attachments []MailAttachment `json:"attachments,omitempty" swaggerignore:"true"`
}

// HasAttachments says whether the request named any.
func (n NoAttachments) HasAttachments() bool { return len(n.Attachments) > 0 }
