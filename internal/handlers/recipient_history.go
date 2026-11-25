package handlers

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// A person's mail history (hub ticket skymail-yeni-tasarim B2): GET
// /recipients/history?email=, every mail SkyMail queued to an address — its
// send, whether it went, whether it was opened or clicked — newest first.
// Every read is recorded before it is answered (recipient_history_views),
// as an opening of masked content is.

// maxAddress is the longest address the history takes, in bytes (RFC 5321's
// 64-octet local part, @ and 255-octet domain).
const maxAddress = 320

var errHistoryAddress = apperrors.ErrValidation.WithParams(map[string]interface{}{
	"email": "must be an address (one @, no tab, line break or NUL, valid UTF-8, at most 320 bytes; encode + as %2B, a blank is read as +)",
})

// RecipientHistoryItem is one mail queued to the address.
type RecipientHistoryItem struct {
	// The queue row.
	ID uuid.UUID `json:"id"`
	// The send (GET /mail_tasks/{id}).
	TaskID uuid.UUID `json:"task_id"`
	// The row's address and name as queued; an account erasure or the recipient retention run empties them, and the row is then no longer found by its address.
	RecipientEmail    string     `json:"recipient_email"`
	RecipientFullName string     `json:"recipient_full_name"`
	RecipientSub      *uuid.UUID `json:"recipient_sub"`
	// pending, processing, sent or failed.
	Status string `json:"status" enums:"pending,processing,sent,failed"`
	// Why it failed or was skipped (suppressed:…, preference: …, reserved_domain:…, cancelled, the relay's reply); null otherwise.
	Error    *string `json:"error"`
	Attempts int     `json:"attempts"`
	// Not sent on purpose (a reserved domain, a suppressed or unsubscribed address, core's no, a cancelled send): failed, never making the send read failed.
	Skipped bool `json:"skipped"`
	// Sent, then a report said the address does not exist (a hard bounce naming the row's own address).
	Undelivered bool `json:"undelivered"`
	// Account mail sent to a suppressed address anyway.
	SuppressedButSent bool       `json:"suppressed_but_sent"`
	QueuedAt          *time.Time `json:"queued_at"`
	// When the relay took it; null when not sent (or sent by an image from before sent_at).
	SentAt *time.Time `json:"sent_at"`
	// The mail's subject as it was rendered; "" once erased.
	Subject string `json:"subject"`
	// available, or deleted: the send's content was emptied (MAIL_CONTENT_RETENTION_DAYS, a sensitive send not kept) or this mail's own body was. GET /mail_tasks/{task_id}/queue/{id}/content opens it while available.
	Content string `json:"content" enums:"available,deleted"`
	// Openings and clicks.
	Tracking RecipientHistoryTracking `json:"tracking"`
	// The send the mail was part of.
	Send RecipientHistorySend `json:"send"`
}

// RecipientHistoryTracking is a mail's openings and clicks, within its
// recipient's 90-day window (older events are gone, as on GET
// /mail_tasks/{id}/tracking/recipients).
type RecipientHistoryTracking struct {
	// The row had a tracking token: its openings and clicks were recorded.
	Tracked bool `json:"tracked"`
	// Why a row of a tracked send was not tracked; null otherwise.
	SkipReason *string `json:"skip_reason"`
	// The first opening, a mail client's prefetch included.
	FirstOpenAt *time.Time `json:"first_open_at"`
	// The first opening or click not marked as likely automated.
	FirstLikelyHumanOpenAt *time.Time `json:"first_likely_human_open_at"`
	Opens                  int        `json:"opens"`
	Clicks                 int        `json:"clicks"`
	LastClickedAt          *time.Time `json:"last_clicked_at"`
}

// RecipientHistorySend is the send a mail of the history was part of.
type RecipientHistorySend struct {
	CreatedAt time.Time `json:"created_at"`
	// Who sent it (the Keycloak subject; skymail for SkyMail's own notices; Silinmiş kullanıcı's subject once erased).
	SentBy string `json:"sent_by"`
	// bulk, single or system, as GET /mail_tasks's kind.
	Kind         string     `json:"kind" enums:"bulk,single,system"`
	Purpose      *string    `json:"purpose" enums:"islem,duyuru"`
	Category     *string    `json:"category"`
	TemplateID   *uuid.UUID `json:"template_id"`
	TemplateName *string    `json:"template_name"`
	TemplateKey  *string    `json:"template_key"`
	MailListID   *uuid.UUID `json:"mail_list_id"`
	MailListName *string    `json:"mail_list_name"`
}

// GetRecipientHistory godoc
//
//	@Summary		List the mail queued to an address
//	@Description	Every mail SkyMail queued to the address, compared whole and case-insensitively, newest first with the row id breaking ties: the send it was part of, whether it went (status, skipped, undelivered), its openings and clicks within the recipient's 90-day window, and whether its content is still kept. Every caller with skymail:mails:read reads every address's history; each read, whose and by whom, is recorded before it is answered (the address as its SHA-256), kept SENT_CONTENT_VIEWS_RETENTION_DAYS. Mail whose address an account erasure or the recipient retention run emptied, and a send that keeps no record of its recipient (erase_after_send), is not found. X-Total-Count counts every row to the address. Never cached.
//	@Tags			Mail
//	@Produce		json
//	@Param			email	query		string	true	"The address (at most 320 bytes)"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		handlers.RecipientHistoryItem
//	@Header			200		{integer}	X-Total-Count		"Number of mails queued to the address"
//	@Failure		400		{object}	apperrors.Problem	"validation_error: email missing or not an address"
//	@Failure		403		{object}	apperrors.Problem	"Forbidden"
//	@Failure		500		{object}	apperrors.Problem	"Internal Server Error"
//	@Router			/recipients/history [get]
func (h *mailHandlerImpl) GetRecipientHistory(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	address, ok := historyAddress(c.Query("email"))
	if !ok {
		return errHistoryAddress
	}
	ctx := c.Context()
	limit, offset := getPaginationParams(c)
	count, err := h.db.CountRecipientHistory(ctx, address)
	if err != nil {
		return err
	}
	rows, err := h.db.ListRecipientHistory(ctx, database.ListRecipientHistoryParams{
		Email: address, RowLimit: limit, RowOffset: offset,
	})
	if err != nil {
		return err
	}
	// No history leaves without the record of who read it.
	actor := versionAuthor(c, database.TemplateAuthorKindOperator)
	if err := h.db.RecordRecipientHistoryView(ctx, database.RecordRecipientHistoryViewParams{
		Email:         address,
		ActorSub:      actor.Sub,
		ActorName:     actor.Name,
		Results:       int(count),
	}); err != nil {
		return err
	}

	items := make([]RecipientHistoryItem, len(rows))
	for i, row := range rows {
		content := "available"
		if row.ContentGone {
			content = "deleted"
		}
		items[i] = RecipientHistoryItem{
			ID:                row.ID,
			TaskID:            row.TaskID,
			RecipientEmail:    row.RecipientEmail,
			RecipientFullName: row.RecipientFullName,
			RecipientSub:      row.RecipientSub,
			Status:            string(row.Status.MailQueueStatus),
			Error:             row.Error,
			Attempts:          row.Attempts,
			Skipped:           row.Skipped,
			Undelivered:       row.Undelivered,
			SuppressedButSent: row.SuppressedButSent,
			QueuedAt:          row.CreatedAt,
			SentAt:            row.SentAt,
			Subject:           row.Subject,
			Content:           content,
			Tracking: RecipientHistoryTracking{
				Tracked:                row.Tracked,
				SkipReason:             row.TrackingSkipReason,
				FirstOpenAt:            scannedTime(row.FirstOpenAt),
				FirstLikelyHumanOpenAt: scannedTime(row.FirstLikelyHumanOpenAt),
				Opens:                  row.OpenCount,
				Clicks:                 row.ClickCount,
				LastClickedAt:          scannedTime(row.LastClickedAt),
			},
			Send: RecipientHistorySend{
				CreatedAt:    row.SendCreatedAt,
				SentBy:       row.SentBy,
				Kind:         row.Kind,
				Purpose:      row.Purpose,
				Category:     row.Category,
				TemplateID:   row.TemplateID,
				TemplateName: row.TemplateName,
				TemplateKey:  row.TemplateKey,
				MailListID:   row.MailListID,
				MailListName: row.MailListName,
			},
		}
	}
	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(items)
}

// historyAddress is the email query trimmed: one @ with something on both
// sides, no tab, line break or NUL, valid UTF-8, at most maxAddress bytes.
func historyAddress(raw string) (string, bool) {
	// An address holds no blank: one inside it is a + sent unencoded
	// (a+b@example.com), which the query string reads as a blank.
	address := strings.ReplaceAll(strings.TrimSpace(raw), " ", "+")
	if address == "" || len(address) > maxAddress || !utf8.ValidString(address) ||
		strings.ContainsAny(address, " \t\r\n\x00") || strings.Count(address, "@") != 1 {
		return "", false
	}
	at := strings.IndexByte(address, '@')
	if at == 0 || at == len(address)-1 {
		return "", false
	}
	return address, true
}

// scannedTime is a timestamptz the database returned untyped (an aggregate
// sqlc cannot type): the time, or nil for NULL.
func scannedTime(v interface{}) *time.Time {
	if t, ok := v.(time.Time); ok {
		return &t
	}
	return nil
}
