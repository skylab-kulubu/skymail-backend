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

// SendStatus is a send's status. A mail task has none of its own; the database
// function mail_task_status derives it from the task's recipients, and it is
// the only definition — the summary shows it and the send list filters by it.
type SendStatus string

const (
	// SendStatusFailed: at least one recipient failed, whatever the others did;
	// or no recipient was queued and a minute has passed since the send.
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

	// The same source values as MailingListItem.
	sourceInternal = "internal"
	sourceKeycloak = "keycloak"
)

// boundedQueryInt reads an optional whole-number query parameter between 1 and
// max, falling back to fallback when it is absent.
func boundedQueryInt(c fiber.Ctx, name string, fallback, max int) (int, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		return 0, apperrors.ErrValidation.WithParams(map[string]interface{}{
			name: fmt.Sprintf("must be a whole number from 1 to %d", max),
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

// QueueCounts counts queue rows — one per recipient of a send — by the status
// the mailer left them in.
type QueueCounts struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
	Sent       int64 `json:"sent"`
	Failed     int64 `json:"failed"`
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
// recipient of a single send. Fields that do not apply to the kind are null.
type SendAudience struct {
	Kind              string     `json:"kind" enums:"mailing_list,single"`
	MailListID        *uuid.UUID `json:"mail_list_id"`
	Name              *string    `json:"name"`
	Source            *string    `json:"source" enums:"internal,keycloak"`
	RecipientFullName *string    `json:"recipient_full_name"`
	RecipientEmail    *string    `json:"recipient_email"`
}

// RecentSend is one of the latest sends on the home screen.
type RecentSend struct {
	ID              uuid.UUID    `json:"id"`
	CreatedAt       time.Time    `json:"created_at"`
	SentBy          string       `json:"sent_by"`
	TemplateID      *uuid.UUID   `json:"template_id"`
	TemplateName    *string      `json:"template_name"`
	TemplateKey     *string      `json:"template_key"`
	Audience        SendAudience `json:"audience"`
	Status          SendStatus   `json:"status"`
	RecipientCounts QueueCounts  `json:"recipient_counts"`
}

// SendSummary is what the home screen shows: the queue as it stands, sends by
// status, the mail sent per day, and the latest sends.
type SendSummary struct {
	TimeZone    string       `json:"time_zone" example:"Europe/Istanbul"`
	QueueCounts QueueCounts  `json:"queue_counts"`
	SendCounts  SendCounts   `json:"send_counts"`
	DailySent   []DailySent  `json:"daily_sent"`
	RecentSends []RecentSend `json:"recent_sends"`
}

// GetSummary godoc
//
//	@Summary		Summarise mail sends
//	@Description	Queue rows (one per recipient) by status; sends by derived status, each equal to the X-Total-Count of the send list filtered by it; mail sent per Europe/Istanbul day over the last days (zero-filled, oldest first, ending today); and the latest sends. Derived status: failed (a recipient failed, or none was queued a minute after the send), sending (none failed and some pending or processing, or none queued yet within that minute), sent (none failed or queued, some sent).
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
	groupNames := h.keycloakGroupNames(c.Context(), sends)
	recentSends := make([]RecentSend, len(sends))
	for i, send := range sends {
		recentSends[i] = RecentSend{
			ID:              send.ID,
			CreatedAt:       send.CreatedAt,
			SentBy:          send.SentBy,
			TemplateID:      send.TemplateID,
			TemplateName:    send.TemplateName,
			TemplateKey:     send.TemplateKey,
			Audience:        sendAudience(send, groupNames),
			Status:          SendStatus(send.Status),
			RecipientCounts: recipientCounts(send),
		}
	}

	return c.JSON(SendSummary{
		TimeZone: summaryTimeZone,
		QueueCounts: QueueCounts{
			Pending:    counts.Pending,
			Processing: counts.Processing,
			Sent:       counts.Sent,
			Failed:     counts.Failed,
		},
		SendCounts: SendCounts{
			Failed:  sendCounts.Failed,
			Sending: sendCounts.Sending,
			Sent:    sendCounts.Sent,
		},
		DailySent:   dailySent,
		RecentSends: recentSends,
	})
}

func recipientCounts(send database.ListMailTaskSendsRow) QueueCounts {
	return QueueCounts{
		Pending:    send.Pending,
		Processing: send.Processing,
		Sent:       send.Sent,
		Failed:     send.Failed,
	}
}

func sendAudience(send database.ListMailTaskSendsRow, groupNames map[uuid.UUID]*string) SendAudience {
	switch {
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

// keycloakGroupNames names the Keycloak groups among the sends. A name is a
// nicety on the home screen, so a group Keycloak cannot name — gone, or
// Keycloak unreachable — is left unnamed rather than failing the summary.
func (h *mailHandlerImpl) keycloakGroupNames(ctx context.Context, sends []database.ListMailTaskSendsRow) map[uuid.UUID]*string {
	names := map[uuid.UUID]*string{}
	for _, send := range sends {
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
