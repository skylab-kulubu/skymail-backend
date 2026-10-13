package handlers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// SystemMailsReadRole reads the content of single mail — the mail a service
// sends one person (POST /mail_tasks/single) — whose template does not hide
// it, beside every send's the other roles let a caller read. It is SkyMail's
// administrator's (Yusuf, 2026-10-12).
const SystemMailsReadRole = "skymail:system-mails:read"

const mailsWriteRole = "skymail:mails:write"

// DefaultContentRetentionDays is how long a send's content is kept when
// MAIL_CONTENT_RETENTION_DAYS does not say: the longest it may say (Yusuf,
// 2026-10-12: "en çok neyse o olsun"). Sensitive content is emptied at once
// whatever it says.
const DefaultContentRetentionDays = MaxContentRetentionDays

// ContentAccess is whether a caller may open a send's content, and if not,
// why. A sensitive send's content (a code or a one-time link) is shown masked;
// a restricted one is shown to others; an expired one is no longer kept.
type ContentAccess string

const (
	ContentAvailable ContentAccess = "available"
	// ContentMasked is a sensitive send's content shown with every code and
	// one-time link masked on the server (Yusuf, 2026-10-13: "kod/bağlantı
	// maskeli, gerisi açık"). The unmasked value never leaves the server.
	ContentMasked ContentAccess = "masked"
	// ContentSensitive is a sensitive send's content that could not be masked
	// for certain (mailer.ErrCannotMask): shown to no one.
	ContentSensitive  ContentAccess = "sensitive"
	ContentRestricted ContentAccess = "restricted"
	ContentExpired    ContentAccess = "expired"
	// ContentNotKept is a sensitive send whose content the content sweep
	// emptied within the retention period: from 2026-10-13 11:16 (skymail-
	// backend #61) until sensitive content was kept, a sensitive send was
	// emptied five minutes after it went out, the sends from before that among
	// them. It does not come back.
	ContentNotKept ContentAccess = "not_kept"
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
	// The template's key: a locked one is sensitive whatever its row says.
	templateKey *string
	sentBy      string
	bulk        bool
	sensitive   bool
	createdAt   time.Time
	clearedAt   *time.Time
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

// contentAccess decides in this order: who asks; then whether the content is
// still kept; then whether it is sensitive, and so shown masked.
func (h *mailHandlerImpl) contentAccess(caller contentCaller, send sendContent) (ContentAccess, *time.Time) {
	if !caller.mayRead(send) {
		return ContentRestricted, nil
	}
	sensitive := send.sensitive || ContentSensitiveLocked(send.templateKey)
	expires := send.createdAt.Add(h.retention())
	if !h.now().Before(expires) {
		return ContentExpired, nil
	}
	if send.clearedAt != nil {
		// Emptied within the retention period: a sensitive send by the sweep
		// that emptied those at once, until 2026-10-13; any other by a shorter
		// MAIL_CONTENT_RETENTION_DAYS than now.
		if sensitive {
			return ContentNotKept, nil
		}
		return ContentExpired, nil
	}
	if sensitive {
		return ContentMasked, &expires
	}
	return ContentAvailable, &expires
}

// SentMailContent is one recipient's mail as it was rendered and sent:
// subject, plain text and HTML, each null unless access is available or
// masked.
type SentMailContent struct {
	ID     uuid.UUID     `json:"id"`
	TaskID uuid.UUID     `json:"task_id"`
	Access ContentAccess `json:"access" enums:"available,masked,sensitive,restricted,expired,not_kept,unavailable"`
	// The subject as the recipient's inbox shows it.
	Subject *string `json:"subject"`
	// The plain-text part.
	PlainText *string `json:"plain_text"`
	// The HTML part, as sent: show it only in a sandboxed frame, with remote
	// content blocked unless the operator asks for it. Masked, each masked
	// value is <mark data-skymail-masked="">••••••</mark> in the text.
	HTML *string `json:"html"`
	// When the content sweep empties it; null when it is not shown.
	ExpiresAt *time.Time `json:"expires_at"`
	// Masked content only: the send's variables masked, by their path (link,
	// code, answers[0].url), and how — variables (rendered again with them
	// masked) or patterns (every link, long number and code in the stored mail
	// masked). Null otherwise.
	MaskedVariables []string `json:"masked_variables"`
	MaskedBy        *string  `json:"masked_by" enums:"variables,patterns"`
}

// GetQueueItemContent godoc
//
//	@Summary		Get one recipient's mail as sent
//	@Description	The subject, plain text and HTML a recipient of the send was sent, rendered for them. access says whether and how they are shown: available — as sent; masked — the send is sensitive (its template carries a code or a one-time link, or it is a single mail whose template does not say it may be shown), and every code and one-time link is replaced by •••••• on the server (masked_variables, masked_by), the opening recorded in sent_content_views; sensitive — it could not be masked for certain, so it is not shown; restricted — shown to the send's sender, for a send to a list, a group or an approved request to holders of skymail:mails:write, and to holders of skymail:system-mails:read; expired — past MAIL_CONTENT_RETENTION_DAYS (3650 by default) and no longer kept; not_kept — a sensitive send whose content was emptied minutes after it went out, before sensitive content was kept (2026-10-13), which does not come back; unavailable — holds nothing (Account erasure emptied it, or it never rendered). The fields are null unless access is available or masked. The HTML may reach the network (images, a tracking pixel): show it sandboxed, with remote content blocked by default.
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
		templateKey: row.TemplateKey,
		sentBy:      row.SentBy,
		bulk:        row.Bulk,
		sensitive:   row.ContentSensitive,
		createdAt:   row.TaskCreatedAt,
		clearedAt:   row.ContentClearedAt,
	})
	answer := SentMailContent{ID: row.ID, TaskID: row.TaskID, Access: access}
	if access != ContentAvailable && access != ContentMasked {
		return c.JSON(answer)
	}
	if row.Subject == "" && row.Body == "" && (row.BodyHtml == nil || *row.BodyHtml == "") {
		answer.Access = ContentUnavailable
		return c.JSON(answer)
	}
	sent := mailer.Rendered{Subject: row.Subject, PlainText: row.Body}
	if row.BodyHtml != nil {
		sent.HTML = *row.BodyHtml
	}
	if access == ContentMasked {
		masked, err := h.maskSentContent(c, row, sent)
		if errors.Is(err, mailer.ErrCannotMask) {
			log.Warn().Str("task_id", row.TaskID.String()).Str("queue_item_id", row.ID.String()).
				Msg("A sensitive mail could not be masked for certain: not shown")
			answer.Access = ContentSensitive
			return c.JSON(answer)
		}
		if err != nil {
			return err
		}
		sent = masked.Rendered
		method := string(masked.Method)
		answer.MaskedVariables = masked.Variables
		if answer.MaskedVariables == nil {
			answer.MaskedVariables = []string{}
		}
		answer.MaskedBy = &method
	}
	answer.Subject = &sent.Subject
	answer.PlainText = &sent.PlainText
	if row.BodyHtml != nil {
		answer.HTML = &sent.HTML
	}
	answer.ExpiresAt = expires
	return c.JSON(answer)
}

// maskedVersionCandidates bounds the template versions tried for the one a
// sensitive send rendered.
const maskedVersionCandidates = 50

// maskSentContent masks a sensitive send's mail for one recipient and records
// who opened it. Nothing is shown unless the opening is recorded.
func (h *mailHandlerImpl) maskSentContent(c fiber.Ctx, row database.GetMailQueueItemContentRow, sent mailer.Rendered) (mailer.MaskedMail, error) {
	var candidates []mailer.TemplateParts
	if row.TemplateSubject != nil && row.TemplatePlainText != nil && row.TemplateHtml != nil {
		candidates = append(candidates, mailer.TemplateParts{
			Subject: *row.TemplateSubject, PlainText: *row.TemplatePlainText, HTML: *row.TemplateHtml,
		})
	}
	if row.TemplateID != nil {
		versions, err := h.db.ListPublishedTemplateVersionParts(c.Context(), database.ListPublishedTemplateVersionPartsParams{
			TemplateID: *row.TemplateID, RowLimit: maskedVersionCandidates,
		})
		if err != nil {
			return mailer.MaskedMail{}, err
		}
		for _, version := range versions {
			candidates = append(candidates, mailer.TemplateParts{
				Subject: version.Subject, PlainText: version.PlainTextContent, HTML: version.HtmlContent,
			})
			// The by-key seed may have kept the row's subject over the version's.
			if row.TemplateSubject != nil && *row.TemplateSubject != version.Subject {
				candidates = append(candidates, mailer.TemplateParts{
					Subject: *row.TemplateSubject, PlainText: version.PlainTextContent, HTML: version.HtmlContent,
				})
			}
		}
	}

	masked, err := mailer.MaskSent(sent, row.BodyVariables, mailer.RecipientInfo{
		FullName: row.RecipientFullName, Email: row.RecipientEmail,
	}, candidates)
	if err != nil {
		return mailer.MaskedMail{}, err
	}

	actor := versionAuthor(c, database.TemplateAuthorKindOperator)
	variables := masked.Variables
	if variables == nil {
		variables = []string{}
	}
	if err := h.db.RecordSentContentView(c.Context(), database.RecordSentContentViewParams{
		TaskID:          row.TaskID,
		QueueItemID:     row.ID,
		ActorSub:        actor.Sub,
		ActorName:       actor.Name,
		MaskedBy:        string(masked.Method),
		MaskedVariables: variables,
	}); err != nil {
		return mailer.MaskedMail{}, err
	}
	return masked, nil
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
