package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// SystemMailsReadRole reads the content of single mail — the mail a service
// sends one person (POST /mail_tasks/single) — whose template does not hide
// it, beside every send's the other roles let a caller read. It is SkyMail's
// administrator's (Yusuf, 2026-10-12).
const SystemMailsReadRole = "skymail:system-mails:read"

const mailsWriteRole = "skymail:mails:write"

// DefaultContentRetentionDays is how long a send's content is kept when
// MAIL_CONTENT_RETENTION_DAYS does not say.
const DefaultContentRetentionDays = 90

// ContentAccess is whether a caller may open a send's content, and if not,
// why. A sensitive send's content is never shown, to anyone; a restricted one
// is shown to others; an expired one is no longer kept.
type ContentAccess string

const (
	ContentAvailable  ContentAccess = "available"
	ContentSensitive  ContentAccess = "sensitive"
	ContentRestricted ContentAccess = "restricted"
	ContentExpired    ContentAccess = "expired"
	// ContentUnavailable is one recipient's mail that holds nothing within the
	// retention period: Account erasure emptied it, or it never rendered.
	ContentUnavailable ContentAccess = "unavailable"
)

// MailHandlerOption sets one of a mail handler's settings.
type MailHandlerOption func(*mailHandlerImpl)

// WithContentRetentionDays sets how long a send's content is shown after it
// was created; the content sweep empties it then.
func WithContentRetentionDays(days int) MailHandlerOption {
	return func(h *mailHandlerImpl) { h.contentRetentionDays = days }
}

func (h *mailHandlerImpl) retention() time.Duration {
	days := h.contentRetentionDays
	if days <= 0 {
		days = DefaultContentRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// contentCaller is who asks: their subject and roles.
type contentCaller struct {
	sub   string
	roles []string
}

func contentCallerOf(c fiber.Ctx) contentCaller {
	caller := contentCaller{}
	caller.sub, _ = c.Locals("user_id").(string)
	caller.roles, _ = c.Locals("roles").([]string)
	return caller
}

func (c contentCaller) has(role string) bool {
	for _, r := range c.roles {
		if r == role {
			return true
		}
	}
	return false
}

// sendContent is what decides who may open a send's content.
type sendContent struct {
	sentBy    string
	bulk      bool
	sensitive bool
	createdAt time.Time
	clearedAt *time.Time
}

// mayRead is the rule (Yusuf, 2026-10-12): a send's own sender; for a send to
// many, anyone who may send to lists; for any send, whoever reads system
// mail.
func (c contentCaller) mayRead(send sendContent) bool {
	switch {
	case c.sub != "" && c.sub == send.sentBy:
		return true
	case c.has(SystemMailsReadRole):
		return true
	default:
		return send.bulk && c.has(mailsWriteRole)
	}
}

// contentAccess decides in this order: sensitive content is shown to no one,
// so who asks does not matter; then who asks; then whether it is still kept.
func (h *mailHandlerImpl) contentAccess(caller contentCaller, send sendContent) (ContentAccess, *time.Time) {
	if send.sensitive {
		return ContentSensitive, nil
	}
	if !caller.mayRead(send) {
		return ContentRestricted, nil
	}
	expires := send.createdAt.Add(h.retention())
	if send.clearedAt != nil || !h.now().Before(expires) {
		return ContentExpired, nil
	}
	return ContentAvailable, &expires
}

// SentMailContent is one recipient's mail as it was rendered and sent:
// subject, plain text and HTML, each null unless access is available.
type SentMailContent struct {
	ID     uuid.UUID     `json:"id"`
	TaskID uuid.UUID     `json:"task_id"`
	Access ContentAccess `json:"access" enums:"available,sensitive,restricted,expired,unavailable"`
	// The subject as the recipient's inbox shows it.
	Subject *string `json:"subject"`
	// The plain-text part.
	PlainText *string `json:"plain_text"`
	// The HTML part, as sent: show it only in a sandboxed frame, with remote
	// content blocked unless the operator asks for it.
	HTML *string `json:"html"`
	// When the content sweep empties it; null when it is not shown.
	ExpiresAt *time.Time `json:"expires_at"`
}

// GetQueueItemContent godoc
//
//	@Summary		Get one recipient's mail as sent
//	@Description	The subject, plain text and HTML a recipient of the send was sent, rendered for them. access says whether they are shown: sensitive — the template carries a code or a one-time link, or it is a single mail whose template does not say it may be shown — is never shown to anyone; restricted is shown to the send's sender, for a send to a list, a group or an approved request to holders of skymail:mails:write, and to holders of skymail:system-mails:read; expired is past MAIL_CONTENT_RETENTION_DAYS (90 by default) and no longer kept; unavailable holds nothing (Account erasure emptied it, or it never rendered). The fields are null unless access is available. The HTML may reach the network (images, a tracking pixel): show it sandboxed, with remote content blocked by default.
//	@Tags			Mail
//	@Produce		json
//	@Param			id		path		string	true	"Task ID"
//	@Param			itemId	path		string	true	"Queue row ID"
//	@Success		200		{object}	handlers.SentMailContent
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/{id}/queue/{itemId}/content [get]
func (h *mailHandlerImpl) GetQueueItemContent(c fiber.Ctx) error {
	taskID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}
	rowID, err := uuid.Parse(c.Params("itemId"))
	if err != nil {
		return err
	}

	row, err := h.db.GetMailQueueItemContent(c.Context(), database.GetMailQueueItemContentParams{TaskID: taskID, ID: rowID})
	if err != nil {
		return err
	}

	access, expires := h.contentAccess(contentCallerOf(c), sendContent{
		sentBy:    row.SentBy,
		bulk:      row.Bulk,
		sensitive: row.ContentSensitive,
		createdAt: row.TaskCreatedAt,
		clearedAt: row.ContentClearedAt,
	})
	answer := SentMailContent{ID: row.ID, TaskID: row.TaskID, Access: access}
	if access != ContentAvailable {
		return c.JSON(answer)
	}
	if row.Subject == "" && row.Body == "" && (row.BodyHtml == nil || *row.BodyHtml == "") {
		answer.Access = ContentUnavailable
		return c.JSON(answer)
	}
	answer.Subject = &row.Subject
	answer.PlainText = &row.Body
	answer.HTML = row.BodyHtml
	answer.ExpiresAt = expires
	return c.JSON(answer)
}

// ContentRetentionEnv names how many days a send's content is kept.
const ContentRetentionEnv = "MAIL_CONTENT_RETENTION_DAYS"

// MaxContentRetentionDays keeps the setting to ten years.
const MaxContentRetentionDays = 3650

// ContentRetentionDaysFromEnv reads MAIL_CONTENT_RETENTION_DAYS, a whole
// number of days from 1 to MaxContentRetentionDays. Unset is
// DefaultContentRetentionDays. Anything else stops startup: a typo must not
// keep personal data longer, or empty it sooner, than was meant.
func ContentRetentionDaysFromEnv(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv(ContentRetentionEnv))
	if raw == "" {
		return DefaultContentRetentionDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > MaxContentRetentionDays {
		return 0, fmt.Errorf("%s must be a whole number of days from 1 to %d, not %q", ContentRetentionEnv, MaxContentRetentionDays, raw)
	}
	return days, nil
}
