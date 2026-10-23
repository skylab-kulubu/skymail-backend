package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// summaryTimeZone is where the club is. "Sent today" on the home screen has to
// be a Turkish operator's today, whatever zone the server runs in.
const summaryTimeZone = "Europe/Istanbul"

// The home screen charts 30 days and lists 5 sends. The bounds keep a request
// to a few index range scans and a short list: a quarter of daily points, and
// no more recent sends than fit a screen.
const (
	defaultSummaryDays   = 30
	maxSummaryDays       = 90
	defaultSummaryRecent = 5
	maxSummaryRecent     = 20
)

// keycloakGroupNameBudget is all the time a response gives Keycloak to name the
// groups among its sends, however many there are.
const keycloakGroupNameBudget = 2 * time.Second

// SendStatus is a send's status. A mail task has none of its own; the database
// function mail_task_status derives it from the task's recipients, and it is
// the only definition — the summary shows it and the send list filters by it.
type SendStatus string

const (
	// SendStatusFailed: at least one recipient failed, whatever the others did,
	// other than one skipped: a reserved domain (reserved_domain) or a
	// suppressed address (suppressed); or every recipient was skipped; or no
	// recipient was queued and a minute has passed since the send.
	SendStatusFailed SendStatus = "failed"
	// SendStatusSending: none failed and at least one is pending or processing;
	// or no recipient is queued yet, within a minute of the send.
	SendStatusSending SendStatus = "sending"
	// SendStatusSent: none failed, none queued, at least one sent.
	SendStatusSent SendStatus = "sent"
)

const (
	audienceMailingList = "mailing_list"
	audienceSingle      = "single"
	// Only a Mail onayı request goes to several people at once; each is sent
	// to on their own, a single send each.
	audiencePeople = "people"

	// The same source values as MailingListItem.
	sourceInternal = "internal"
	sourceKeycloak = "keycloak"
)

// boundedQueryInt reads an optional whole-number query parameter between 1 and
// upper, falling back to fallback when it is absent.
func boundedQueryInt(c fiber.Ctx, name string, fallback, upper int) (int, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > upper {
		return 0, apperrors.ErrValidation.WithParams(map[string]interface{}{
			name: fmt.Sprintf("must be a whole number from 1 to %d", upper),
		})
	}
	return value, nil
}

// parseSendStatusFilter reads the send list's status filter; an empty value
// lists every send.
func parseSendStatusFilter(raw string) (*string, error) {
	status := SendStatus(strings.ToLower(strings.TrimSpace(raw)))
	switch status {
	case "":
		return nil, nil
	case SendStatusFailed, SendStatusSending, SendStatusSent:
		value := string(status)
		return &value, nil
	default:
		return nil, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"status": "must be one of failed, sending, sent",
		})
	}
}

// parseRecipientStatusFilter reads a send's recipient filter: a queue status,
// not a send's derived one. An empty value lists every recipient.
func parseRecipientStatusFilter(raw string) (database.NullMailQueueStatus, error) {
	status := database.MailQueueStatus(strings.ToLower(strings.TrimSpace(raw)))
	switch status {
	case "":
		return database.NullMailQueueStatus{}, nil
	case database.MailQueueStatusPending, database.MailQueueStatusProcessing,
		database.MailQueueStatusSent, database.MailQueueStatusFailed:
		return database.NullMailQueueStatus{MailQueueStatus: status, Valid: true}, nil
	default:
		return database.NullMailQueueStatus{}, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"status": "must be one of pending, processing, sent, failed",
		})
	}
}

// QueueCounts counts queue rows — one per recipient of a send — by the status
// the mailer left them in.
type QueueCounts struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
	Sent       int64 `json:"sent"`
	Failed     int64 `json:"failed"`
}

// QueueTotals is a send's recipients by status, and how many of them the
// suppression list touched: suppressed (written failed, never sent: their
// address is suppressed; counted in failed too), suppressed_but_sent (account
// mail sent to a suppressed address anyway, flagged; counted in their status)
// and undelivered (sent, then a delivery status notification said the
// address does not exist; counted in sent).
type QueueTotals struct {
	QueueCounts
	// Written failed when queued, never sent: the address is suppressed (in failed too).
	Suppressed int64 `json:"suppressed"`
	// Account mail sent to a suppressed address anyway, flagged (in its status too).
	SuppressedButSent int64 `json:"suppressed_but_sent"`
	// Sent, then a delivery status notification said the address does not exist (in sent too).
	Undelivered int64 `json:"undelivered"`
}

// SendCounts counts sends by their derived status. Each number equals the
// X-Total-Count of the send list filtered by that status.
type SendCounts struct {
	Failed  int64 `json:"failed"`
	Sending int64 `json:"sending"`
	Sent    int64 `json:"sent"`
}

// DailySent is the mail sent on one calendar day in the summary's time zone.
type DailySent struct {
	Date string `json:"date" example:"2026-09-22"`
	Sent int64  `json:"sent"`
}

// SendAudience is who a send went to: a mailing list — an internal one or a
// Keycloak group, told apart by source as in the mailing list API — or the one
// recipient of a single send, or a fixed Forms snapshot. Fields that do not
// apply to the kind are null; Forms-only fields are omitted on legacy audiences.
//
// A Mail onayı request's audience is this type too, so a request reads as the
// send it becomes, and one to several people is people, its recipients naming
// them. A send is never people: approving such a request queues one single
// send per person. A form source queues one bulk send for its fixed snapshot.
type SendAudience struct {
	// Present only for kind=form: the persistent source ID, distinct from form_id.
	FormSourceID *uuid.UUID `json:"form_source_id,omitempty"`
	// The original Forms form; clients construct the link from a fixed trusted origin.
	FormID *uuid.UUID `json:"form_id,omitempty"`
	// The original snapshot count, fixed when Forms prepared the source.
	RecipientCount *int       `json:"recipient_count,omitempty"`
	PreparedAt     *time.Time `json:"prepared_at,omitempty"`
	// mailing_list, single, form, or — only on a Mail onayı request — people.
	Kind              string     `json:"kind" enums:"mailing_list,single,people,form"`
	MailListID        *uuid.UUID `json:"mail_list_id"`
	Name              *string    `json:"name"`
	Source            *string    `json:"source" enums:"internal,keycloak,forms"`
	RecipientFullName *string    `json:"recipient_full_name"`
	RecipientEmail    *string    `json:"recipient_email"`
}

// SendSummary is what the home screen shows: the queue as it stands, whether
// the sender is paused, sends by status, the mail sent per day, and the latest
// sends.
type SendSummary struct {
	TimeZone    string      `json:"time_zone" example:"Europe/Istanbul"`
	QueueCounts QueueCounts `json:"queue_counts"`
	// True while MAIL_SENDER=paused holds the sender back, as after a restore
	// from backup: mail is queued and stays pending, and none is sent until
	// the variable is removed and SkyMail redeployed.
	SenderPaused bool `json:"sender_paused"`
	// True while SKYMAIL_TRACKING=on: a send may ask for open and click tracking. The send form shows its tracking box only then.
	TrackingAvailable bool `json:"tracking_available"`
	// True while SKYMAIL_TRACKING_OPERATOR=on with SKYMAIL_TRACKING=on: every person's send (a list, a group, people, a Forms source, a Mail onayı request) is tracked whether it asked or not, of any template. The send form then shows no tracking box: a send's tracking field is ignored.
	TrackingAlways bool `json:"tracking_always"`
	// True while SKYMAIL_MAIL_PURPOSE=on: a send names its purpose (islem or duyuru) and a duyuru send its category, and the purpose picks the mailbox it goes out from. The send form shows its purpose, category and statements only then; while it is false they are ignored.
	PurposeAvailable bool           `json:"purpose_available"`
	SendCounts       SendCounts     `json:"send_counts"`
	DailySent        []DailySent    `json:"daily_sent"`
	RecentSends      []MailTaskItem `json:"recent_sends"`
}

// TrackingMode is how SkyMail tracks a person's send: what the send form needs
// to show its box, or to say that every send is tracked. It names no send and
// no person.
type TrackingMode struct {
	// True while SKYMAIL_TRACKING=on: a send may ask for open and click tracking.
	TrackingAvailable bool `json:"tracking_available"`
	// True while SKYMAIL_TRACKING_OPERATOR=on too: every person's send is tracked, the send form shows no box and a send's tracking field is ignored.
	TrackingAlways bool `json:"tracking_always"`
	// True while SKYMAIL_MAIL_PURPOSE=on: a send names its purpose (islem or duyuru), and a duyuru send its category; tracking follows tracking_available and tracking_always whatever the purpose. False: the fields are ignored.
	PurposeAvailable bool `json:"purpose_available"`
}

// GetTrackingMode godoc
//
//	@Summary		How a person's send is tracked
//	@Description	tracking_available, tracking_always and purpose_available, as the summary says them, for anyone who may use SkyMail: someone who only submits a send for approval sees whether it will be tracked and whether it names a purpose. It is a setting of SkyMail, not of any send.
//	@Tags			Mail
//	@Produce		json
//	@Success		200	{object}	handlers.TrackingMode
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Router			/mail_tasks/tracking-mode [get]
func (h *mailHandlerImpl) GetTrackingMode(c fiber.Ctx) error {
	return c.JSON(TrackingMode{TrackingAvailable: h.mailer.TrackingAvailable(), TrackingAlways: h.mailer.OperatorTrackingAlways(), PurposeAvailable: h.mailer.PurposeAvailable()})
}

// GetSummary godoc
//
//	@Summary		Summarise mail sends
//	@Description	Queue rows (one per recipient) by status; sender_paused, true while MAIL_SENDER=paused holds the sender back (after a restore from backup: mail is queued and stays pending, none is sent); tracking_available, true while SKYMAIL_TRACKING=on (a send may ask for open and click tracking); tracking_always, true while SKYMAIL_TRACKING_OPERATOR=on too (every person's send is tracked, the field is ignored, the send form shows no box); sends by derived status, each equal to the X-Total-Count of the send list filtered by it; mail sent per Europe/Istanbul day over the last days (zero-filled, oldest first, ending today); and the latest sends. Derived status: failed (a recipient failed, other than one skipped: a reserved domain (reserved_domain) or a suppressed address (suppressed); every recipient was skipped; or none was queued a minute after the send), sending (none failed and some pending or processing, or none queued yet within that minute), sent (none failed or queued, some sent).
//	@Tags			Mail
//	@Produce		json
//	@Param			days	query		int	false	"Days in the daily series, ending today in Europe/Istanbul (1-90)"	default(30)
//	@Param			recent	query		int	false	"Latest sends to list, newest first (1-20)"							default(5)
//	@Success		200		{object}	handlers.SendSummary
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mail_tasks/summary [get]
func (h *mailHandlerImpl) GetSummary(c fiber.Ctx) error {
	days, err := boundedQueryInt(c, "days", defaultSummaryDays, maxSummaryDays)
	if err != nil {
		return err
	}
	recent, err := boundedQueryInt(c, "recent", defaultSummaryRecent, maxSummaryRecent)
	if err != nil {
		return err
	}

	counts, err := h.db.CountMailQueueByStatus(c.Context())
	if err != nil {
		return err
	}

	sendCounts, err := h.db.CountMailTasksByStatus(c.Context())
	if err != nil {
		return err
	}

	series, err := h.db.GetDailySentCounts(c.Context(), database.GetDailySentCountsParams{
		TimeZone: summaryTimeZone,
		AsOf:     h.now(),
		Days:     days,
	})
	if err != nil {
		return err
	}
	dailySent := make([]DailySent, len(series))
	for i, day := range series {
		dailySent[i] = DailySent{Date: day.Day.Format("2006-01-02"), Sent: day.Sent}
	}

	sends, err := h.db.ListMailTaskSends(c.Context(), database.ListMailTaskSendsParams{
		Limit: int32(recent),
	})
	if err != nil {
		return err
	}

	return c.JSON(SendSummary{
		TimeZone: summaryTimeZone,
		QueueCounts: QueueCounts{
			Pending:    counts.Pending,
			Processing: counts.Processing,
			Sent:       counts.Sent,
			Failed:     counts.Failed,
		},
		SenderPaused:      h.mailer.SenderPaused(),
		TrackingAvailable: h.mailer.TrackingAvailable(),
		TrackingAlways:    h.mailer.OperatorTrackingAlways(),
		PurposeAvailable:  h.mailer.PurposeAvailable(),
		SendCounts: SendCounts{
			Failed:  sendCounts.Failed,
			Sending: sendCounts.Sending,
			Sent:    sendCounts.Sent,
		},
		DailySent:   dailySent,
		RecentSends: h.mailTaskItems(c.Context(), contentCallerOf(c), sends),
	})
}

// mailTaskItems shapes sends the one way every screen shows them, naming the
// Keycloak groups among them in one bounded lookup.
func (h *mailHandlerImpl) mailTaskItems(ctx context.Context, caller contentCaller, sends []database.ListMailTaskSendsRow) []MailTaskItem {
	groupNames := h.keycloakGroupNames(ctx, sends)
	items := make([]MailTaskItem, len(sends))
	for i, send := range sends {
		access, expires := h.contentAccess(caller, sendContent{
			sentBy:    send.SentBy,
			bulk:      send.Bulk,
			sensitive: send.ContentSensitive,
			createdAt: send.CreatedAt,
			clearedAt: send.ContentClearedAt,
			maskedAt:  send.ContentMaskedAt,
		})
		// A masked send's values are masked too, but they are listed only as
		// sent: its content is opened one recipient at a time, and recorded
		// (GetQueueItemContent).
		var variables []byte
		if access == ContentAvailable {
			variables = send.BodyVariables
		}
		items[i] = MailTaskItem{
			ID:               send.ID,
			SentBy:           send.SentBy,
			TemplateID:       send.TemplateID,
			MailListID:       send.MailListID,
			BodyVariables:    variables,
			ContentAccess:    access,
			ContentExpiresAt: expires,
			CreatedAt:        send.CreatedAt,
			TemplateName:     send.TemplateName,
			MailListName:     send.MailListName,
			TemplateKey:      send.TemplateKey,
			Status:           SendStatus(send.Status),
			RecipientCounts:  recipientCounts(send),
			Audience:         sendAudience(send, groupNames),
			Tracking:         send.TrackingRequested,
			SendPurposeFields: SendPurposeFields{
				Purpose:                 send.Purpose,
				Category:                send.Category,
				TransactionalStatement:  send.TransactionalStatement,
				CommercialContentAbsent: send.CommercialContentAbsent,
			},
		}
	}
	return items
}

func recipientCounts(send database.ListMailTaskSendsRow) QueueTotals {
	return QueueTotals{
		QueueCounts: QueueCounts{
			Pending:    send.Pending,
			Processing: send.Processing,
			Sent:       send.Sent,
			Failed:     send.Failed,
		},
		Suppressed:        send.Suppressed,
		SuppressedButSent: send.SuppressedButSent,
		Undelivered:       send.Undelivered,
	}
}

func sendAudience(send database.ListMailTaskSendsRow, groupNames map[uuid.UUID]*string) SendAudience {
	switch {
	case send.FormSourceID != nil:
		source := "forms"
		return SendAudience{Kind: "form", Source: &source, Name: send.FormTitle, FormSourceID: send.FormSourceID,
			FormID: send.FormID, RecipientCount: send.FormRecipientCount, PreparedAt: send.FormPreparedAt}
	case send.MailListID == nil:
		return SendAudience{
			Kind:              audienceSingle,
			RecipientFullName: send.SingleRecipientFullName,
			RecipientEmail:    send.SingleRecipientEmail,
		}
	case send.InternalMailList:
		source := sourceInternal
		return SendAudience{Kind: audienceMailingList, MailListID: send.MailListID, Name: send.MailListName, Source: &source}
	default:
		source := sourceKeycloak
		return SendAudience{Kind: audienceMailingList, MailListID: send.MailListID, Name: groupNames[*send.MailListID], Source: &source}
	}
}

// keycloakGroupNames names the Keycloak groups among the sends, asking once
// per group. A name is a nicety, so a group Keycloak cannot name — gone, or
// Keycloak unreachable or too slow — is left unnamed rather than failing or
// holding up the response.
func (h *mailHandlerImpl) keycloakGroupNames(ctx context.Context, sends []database.ListMailTaskSendsRow) map[uuid.UUID]*string {
	ctx, cancel := context.WithTimeout(ctx, keycloakGroupNameBudget)
	defer cancel()

	names := map[uuid.UUID]*string{}
	for _, send := range sends {
		if ctx.Err() != nil {
			break
		}
		if send.MailListID == nil || send.InternalMailList {
			continue
		}
		id := *send.MailListID
		if _, done := names[id]; done {
			continue
		}
		names[id] = nil
		group, err := h.kc.GetGroup(ctx, id.String())
		if err != nil {
			log.Warn().Err(err).Str("group_id", id.String()).Msg("could not name the Keycloak group of a recent send")
			continue
		}
		if group != nil && group.Name != nil {
			names[id] = group.Name
		}
	}
	return names
}
