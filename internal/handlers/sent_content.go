package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
	"github.com/skylab-kulubu/skynotify-backend/internal/mailer"
)

// SystemMailsReadRole reads the content of single mail — the mail a service
// sends one person (POST /mail_tasks/single) — whose template does not hide
// it, beside every send's the other roles let a caller read. It is SkyNotify's
// administrator's (Yusuf, 2026-10-12).
const SystemMailsReadRole = "skynotify:system-mails:read"

const mailsWriteRole = "skynotify:mails:write"

// DefaultContentRetentionDays is how long a send's content is kept when
// MAIL_CONTENT_RETENTION_DAYS does not say: a year, what production is set
// to (KVKK notice s6; hub ticket kvkk-s6-yayin-oncesi 15; it replaces Yusuf's
// 2026-10-12 "the longest allowed"). Sensitive content is masked whatever it
// says.
const DefaultContentRetentionDays = 365

// ContentAccess is whether a caller may open a send's content, and if not,
// why. A sensitive send's content is kept masked and shown so; a restricted
// one is shown to others; an expired one is no longer kept.
type ContentAccess string

const (
	ContentAvailable ContentAccess = "available"
	// ContentMasked is a sensitive send's content as the content sweep kept
	// it: every code, one-time link and IP address replaced by •••••• (Yusuf,
	// 2026-10-13: "kod/bağlantı maskeli, gerisi açık"). The raw value is not
	// kept.
	ContentMasked ContentAccess = "masked"
	// ContentPending is a sensitive send the sweep has not masked yet: some of
	// its rows are still to go out, or it went out in the last minutes. Its
	// content is not shown until it is masked.
	ContentPending ContentAccess = "pending"
	// ContentSensitive is what a backend from before masking answered for a
	// sensitive send; this one does not.
	ContentSensitive  ContentAccess = "sensitive"
	ContentRestricted ContentAccess = "restricted"
	ContentExpired    ContentAccess = "expired"
	// ContentNotKept is a sensitive send whose content was emptied within the
	// retention period: from 2026-10-13 11:16 (skynotify-backend #61) until
	// sensitive content was kept masked, a sensitive send was emptied five
	// minutes after it went out, the sends from before that among them; and a
	// send the sweep could not mask for certain. It does not come back.
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
	sentBy    string
	bulk      bool
	sensitive bool
	createdAt time.Time
	clearedAt *time.Time
	maskedAt  *time.Time
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
// still kept; then whether it is kept masked, still to be masked, or as sent.
// sensitive is mail_task_content_sensitive(), which counts the locked keys.
func (h *mailHandlerImpl) contentAccess(caller contentCaller, send sendContent) (ContentAccess, *time.Time) {
	if !caller.mayRead(send) {
		return ContentRestricted, nil
	}
	expires := send.createdAt.Add(h.retention())
	if !h.now().Before(expires) {
		return ContentExpired, nil
	}
	if send.clearedAt != nil {
		// Emptied within the retention period: a sensitive send by the sweep
		// that emptied those at once until 2026-10-13, or one it could not
		// mask; any other by a shorter MAIL_CONTENT_RETENTION_DAYS than now.
		if send.sensitive {
			return ContentNotKept, nil
		}
		return ContentExpired, nil
	}
	switch {
	case send.maskedAt != nil:
		return ContentMasked, &expires
	case send.sensitive:
		return ContentPending, nil
	default:
		return ContentAvailable, &expires
	}
}

// SentMailContent is one recipient's mail as it is kept: subject, plain text
// and HTML, each null unless access is available or masked.
type SentMailContent struct {
	ID     uuid.UUID     `json:"id"`
	TaskID uuid.UUID     `json:"task_id"`
	Access ContentAccess `json:"access" enums:"available,masked,pending,restricted,expired,not_kept,unavailable"`
	// The subject as the recipient's inbox shows it.
	Subject *string `json:"subject"`
	// The plain-text part.
	PlainText *string `json:"plain_text"`
	// The HTML part: show it only in a sandboxed frame, with remote content
	// blocked unless the operator asks for it. Masked, each masked value in its
	// text is <mark data-skynotify-masked="">••••••</mark>.
	HTML *string `json:"html"`
	// When the content sweep empties it; null when it is not shown.
	ExpiresAt *time.Time `json:"expires_at"`
	// Masked content only: the send's variables masked, by their path (link,
	// code, answers[0].url), and how — variables (rendered again with them
	// masked) or patterns (every link, IP address, long number and code in the
	// stored mail masked). Null otherwise.
	MaskedVariables []string `json:"masked_variables"`
	MaskedBy        *string  `json:"masked_by" enums:"variables,patterns"`
}

// GetQueueItemContent godoc
//
//	@Summary		Get one recipient's mail as kept
//	@Description	The subject, plain text and HTML a recipient of the send was sent, as SkyNotify keeps it. access says whether and how they are shown: available — as sent; masked — the send is sensitive (its template carries a code or a one-time link, or said so when it was sent, or it is a single mail whose template does not say it may be shown), and the content sweep replaced every code, one-time link and IP address with •••••• once the send was done (masked_variables, masked_by); the unmasked mail is not kept, and each opening is recorded in sent_content_views; pending — sensitive and not masked yet (a row still to go out, or sent in the last minutes): not shown; restricted — shown to the send's sender, for a send to a list, a group or an approved request to holders of skynotify:mails:write, and to holders of skynotify:system-mails:read; expired — past MAIL_CONTENT_RETENTION_DAYS (365 by default) and no longer kept; not_kept — a sensitive send whose content was emptied (before sensitive content was kept, 2026-10-13, or because it could not be masked for certain), which does not come back; unavailable — holds nothing (Account erasure emptied it, or it never rendered). The fields are null unless access is available or masked. The HTML may reach the network (images, a tracking pixel): show it sandboxed, with remote content blocked by default. Never cached.
//	@Tags			Mail
//	@Produce		json
//	@Param			id		path		string	true	"Task ID"
//	@Param			itemId	path		string	true	"Queue row ID"
//	@Success		200		{object}	handlers.SentMailContent
//	@Failure		400		{object}	apperrors.Problem	"Bad Request"
//	@Failure		403		{object}	apperrors.Problem	"Forbidden"
//	@Failure		404		{object}	apperrors.Problem	"Not Found"
//	@Failure		500		{object}	apperrors.Problem	"Internal Server Error"
//	@Router			/mail_tasks/{id}/queue/{itemId}/content [get]
func (h *mailHandlerImpl) GetQueueItemContent(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
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
		maskedAt:  row.ContentMaskedAt,
	})
	answer := SentMailContent{ID: row.ID, TaskID: row.TaskID, Access: access}
	if access != ContentAvailable && access != ContentMasked {
		return c.JSON(answer)
	}
	if row.Subject == "" && row.Body == "" && (row.BodyHtml == nil || *row.BodyHtml == "") {
		answer.Access = ContentUnavailable
		return c.JSON(answer)
	}
	if access == ContentMasked {
		answer.MaskedVariables = row.ContentMaskedVariables
		if answer.MaskedVariables == nil {
			answer.MaskedVariables = []string{}
		}
		answer.MaskedBy = row.ContentMaskedBy
		// No content leaves without the record of who opened it.
		actor := versionAuthor(c, database.TemplateAuthorKindOperator)
		maskedBy := string(mailer.MaskedByPatterns)
		if row.ContentMaskedBy != nil {
			maskedBy = *row.ContentMaskedBy
		}
		if err := h.db.RecordSentContentView(c.Context(), database.RecordSentContentViewParams{
			TaskID:          row.TaskID,
			QueueItemID:     row.ID,
			ActorSub:        actor.Sub,
			ActorName:       actor.Name,
			MaskedBy:        maskedBy,
			MaskedVariables: answer.MaskedVariables,
		}); err != nil {
			return err
		}
	}
	answer.Subject = &row.Subject
	answer.PlainText = &row.Body
	answer.HTML = row.BodyHtml
	answer.ExpiresAt = expires
	return c.JSON(answer)
}

// AuditReadRole reads who opened a masked mail's content: the record every
// opening writes (sent_content_views). It is apart from the roles that open
// the content — system-mails:read, mails:write, the send's own sender — so
// those audited do not read their own record (Yusuf, 2026-11-22: DK alone;
// Microsoft Purview's Audit Reader, NIST SP 800-53 AU-9(4)).
const AuditReadRole = "skynotify:audit:read"

// ContentViewer is who opened a masked mail, as their token named them then.
// Account erasure replaces both with database.DeletedUserSubject and
// database.DeletedUserName.
type ContentViewer struct {
	Sub  *string `json:"sub"`
	Name *string `json:"name"`
}

// SentContentView is one opening of one recipient's masked mail. It names the
// queue row, never the recipient.
type SentContentView struct {
	ID uuid.UUID `json:"id"`
	// The queue row whose mail was opened (GET /mail_tasks/{id}/queue).
	QueueItemID uuid.UUID     `json:"queue_item_id"`
	Actor       ContentViewer `json:"actor"`
	// How the mail was masked when it was opened, and the variables masked.
	MaskedBy        string    `json:"masked_by" enums:"variables,patterns"`
	MaskedVariables []string  `json:"masked_variables"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListContentViews godoc
//
//	@Summary		List who opened a send's masked content
//	@Description	Every opening of a recipient's masked mail of the send (GET /mail_tasks/{id}/queue/{itemId}/content answering masked), newest first: who, when, which queue row, how it was masked. Names no recipient. Takes skynotify:audit:read, which the roles that open the content do not carry. An erased account reads as Silinmiş kullanıcı. Records older than SENT_CONTENT_VIEWS_RETENTION_DAYS are gone. Never cached.
//	@Tags			Mail
//	@Produce		json
//	@Param			id		path		string	true	"Task ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		handlers.SentContentView
//	@Header			200		{integer}	X-Total-Count		"Number of openings"
//	@Failure		400		{object}	apperrors.Problem	"Bad Request"
//	@Failure		403		{object}	apperrors.Problem	"Forbidden"
//	@Failure		404		{object}	apperrors.Problem	"Not Found"
//	@Failure		500		{object}	apperrors.Problem	"Internal Server Error"
//	@Router			/mail_tasks/{id}/content-views [get]
func (h *mailHandlerImpl) ListContentViews(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	taskID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.ErrBadRequest
	}
	if _, err := h.db.GetMailTaskById(c.Context(), taskID); err != nil {
		return err
	}
	limit, offset := getPaginationParams(c)
	rows, err := h.db.ListSentContentViews(c.Context(), database.ListSentContentViewsParams{
		TaskID: taskID, RowLimit: limit, RowOffset: offset,
	})
	if err != nil {
		return err
	}
	count, err := h.db.CountSentContentViews(c.Context(), taskID)
	if err != nil {
		return err
	}
	views := make([]SentContentView, len(rows))
	for i, row := range rows {
		masked := row.MaskedVariables
		if masked == nil {
			masked = []string{}
		}
		views[i] = SentContentView{
			ID:              row.ID,
			QueueItemID:     row.QueueItemID,
			Actor:           ContentViewer{Sub: row.ActorSub, Name: row.ActorName},
			MaskedBy:        row.MaskedBy,
			MaskedVariables: masked,
			CreatedAt:       row.CreatedAt,
		}
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(views)
}

// ViewRetentionEnv names how many days a record of who opened a masked mail
// (sent_content_views) is kept.
const ViewRetentionEnv = "SENT_CONTENT_VIEWS_RETENTION_DAYS"

// DefaultViewRetentionDays is how long a record of who opened a masked mail is
// kept when SENT_CONTENT_VIEWS_RETENTION_DAYS does not say: two years (Yusuf,
// 2026-10-14).
const DefaultViewRetentionDays = 730

// ViewRetentionDaysFromEnv reads SENT_CONTENT_VIEWS_RETENTION_DAYS, a whole
// number of days from 1 to MaxContentRetentionDays. Unset is
// DefaultViewRetentionDays. Anything else stops startup.
func ViewRetentionDaysFromEnv(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv(ViewRetentionEnv))
	if raw == "" {
		return DefaultViewRetentionDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > MaxContentRetentionDays {
		return 0, fmt.Errorf("%s must be a whole number of days from 1 to %d, not %q", ViewRetentionEnv, MaxContentRetentionDays, raw)
	}
	return days, nil
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
