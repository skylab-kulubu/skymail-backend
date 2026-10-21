package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

const TrackingReadRole = "skymail:mails:tracking:read"
const trackingReadBudget = 2 * time.Second

// TrackingSummary has no identities or target URLs. A year after the last
// mail went out (tracking.EventRetention) report is a retained
// >=10-recipient aggregate, or null when there is no such total.
type TrackingSummary struct {
	TrackingAvailable bool               `json:"tracking_available"`
	TrackingRequested bool               `json:"tracking_requested"`
	Retained          bool               `json:"retained"`
	Report            *TrackingAggregate `json:"report"`
}

type TrackingRecipientPage struct {
	TrackingAvailable bool                `json:"tracking_available"`
	Page              int                 `json:"page"`
	PageSize          int                 `json:"page_size"`
	Total             int                 `json:"total"`
	Items             []TrackingRecipient `json:"items"`
}

// TrackingAggregate is the public summary, without recipient identity.
type TrackingAggregate struct {
	TrackedRecipients       int                  `json:"tracked_recipients"`
	UntrackedByReason       map[string]int       `json:"untracked_by_reason"`
	UniqueOpens             int                  `json:"unique_opens"`
	UniqueLikelyHumanOpens  int                  `json:"unique_likely_human_opens"`
	AutomatedOnlyRecipients *int                 `json:"automated_only_recipients,omitempty"`
	UniqueClickers          int                  `json:"unique_clickers"`
	ClicksByLink            []TrackingLinkCounts `json:"clicks_by_link"`
	Series                  *[]TrackingPoint     `json:"series,omitempty"`
	RolledUpAt              *time.Time           `json:"rolled_up_at,omitempty"`
}
type TrackingLinkCounts struct {
	ID             int64 `json:"id"`
	Position       int   `json:"position"`
	Clicks         int   `json:"clicks"`
	UniqueClickers int   `json:"unique_clickers"`
}
type TrackingPoint struct {
	At                time.Time `json:"at"`
	Interval          string    `json:"interval"`
	LikelyHumanOpens  int       `json:"likely_human_opens"`
	AutomatedOpens    int       `json:"automated_opens"`
	LikelyHumanClicks int       `json:"likely_human_clicks"`
	AutomatedClicks   int       `json:"automated_clicks"`
}
type TrackingRecipient struct {
	ID                     uuid.UUID               `json:"id"`
	RecipientSub           *uuid.UUID              `json:"recipient_sub"`
	RecipientEmail         string                  `json:"recipient_email"`
	RecipientFullName      string                  `json:"recipient_full_name"`
	Tracked                bool                    `json:"tracked"`
	TrackingSkipReason     *string                 `json:"tracking_skip_reason"`
	FirstOpenAt            *time.Time              `json:"first_open_at"`
	FirstLikelyHumanOpenAt *time.Time              `json:"first_likely_human_open_at"`
	LikelyHumanOpenSource  *string                 `json:"likely_human_open_source"`
	OpenCount              int                     `json:"open_count"`
	LastClickedAt          *time.Time              `json:"last_clicked_at"`
	AutomatedOnly          bool                    `json:"automated_only"`
	SourceClasses          []string                `json:"source_classes"`
	ClickedLinks           []TrackingRecipientLink `json:"clicked_links"`
}
type TrackingRecipientLink struct {
	LinkID        int64     `json:"link_id"`
	Clicks        int       `json:"clicks"`
	LastClickedAt time.Time `json:"last_clicked_at"`
}

// GetTracking godoc
//
//	@Summary		Tracking counts, per-link counts and hour/day series
//	@Description	Requires skymail:mails:read. No identities, link tokens or target URLs. After a year only retained totals for at least 10 tracked recipients, or null. tracking_available follows the current flag.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Mail task ID"
//	@Success		200	{object}	handlers.TrackingSummary
//	@Failure		403	{object}	apperrors.AppError
//	@Failure		404	{object}	apperrors.AppError
//	@Router			/mail_tasks/{id}/tracking [get]
func (h *mailHandlerImpl) GetTracking(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.ErrBadRequest
	}
	ctx, cancel := context.WithTimeout(c.Context(), trackingReadBudget)
	defer cancel()
	task, err := h.db.TrackingTaskAccess(ctx, id)
	if err != nil {
		return err
	}
	retained := task.TrackingClosedAt != nil || !task.SentAt.Add(tracking.EventRetention).After(h.now())
	var report []byte
	if retained {
		report, err = h.db.GetRetainedTrackingReport(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		}
	} else {
		report, err = h.db.GetTrackingReport(ctx, database.GetTrackingReportParams{TaskID: id, SentAt: task.StartedAt})
	}
	if err != nil {
		return err
	}
	var aggregate *TrackingAggregate
	if len(report) > 0 {
		aggregate = &TrackingAggregate{}
		if err := json.Unmarshal(report, aggregate); err != nil {
			return err
		}
	}
	return c.JSON(TrackingSummary{TrackingAvailable: h.mailer.TrackingAvailable(), TrackingRequested: task.TrackingRequested, Retained: retained, Report: aggregate})
}

// GetTrackingRecipients godoc
//
//	@Summary		Recipient opening and click observations
//	@Description	Requires skymail:mails:tracking:read or the send's own sender (sent_by equals token sub, a person's token: a service's token reads the mail it sent only with the role). First likely human opening falls back to a non-automated click, marked click_inferred. Returns 410 a year after the last mail went out, even if retention has not run. Does not return tracking tokens or target URLs.
//	@Tags			Mail
//	@Produce		json
//	@Param			id			path		string	true	"Mail task ID"
//	@Param			filter		query		string	false	"opened, not_opened, clicked, automated_only"
//	@Param			page		query		int		false	"Page (1–1000000)"
//	@Param			page_size	query		int		false	"Rows per page (1–100, default 25)"
//	@Success		200			{object}	handlers.TrackingRecipientPage
//	@Failure		403			{object}	apperrors.AppError
//	@Failure		404			{object}	apperrors.AppError
//	@Failure		410			{object}	apperrors.AppError
//	@Router			/mail_tasks/{id}/tracking/recipients [get]
func (h *mailHandlerImpl) GetTrackingRecipients(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.ErrBadRequest
	}
	ctx, cancel := context.WithTimeout(c.Context(), trackingReadBudget)
	defer cancel()
	task, err := h.db.TrackingTaskAccess(ctx, id)
	if err != nil {
		return err
	}
	// The sender reads their own send's people; a service is no sender here:
	// the mail it sends (SKYMAIL_TRACKING_SYSTEM) is read with the role only.
	caller := contentCallerOf(c)
	sender := caller.sub != "" && caller.sub == task.SentBy && !middlewares.IsServiceAccount(c)
	if !caller.has(TrackingReadRole) && !sender {
		return apperrors.ErrForbidden
	}
	if task.TrackingClosedAt != nil || !task.SentAt.Add(tracking.EventRetention).After(h.now()) {
		return apperrors.New("mail.tracking_expired", "Kişi bazında takip kaydı 1 yıl sonra silindi.", fiber.StatusGone)
	}
	filter := c.Query("filter")
	switch filter {
	case "", "opened", "not_opened", "clicked", "automated_only":
	default:
		return fiber.ErrBadRequest
	}
	page, err := boundedQueryInt(c, "page", 1, 1000000)
	if err != nil {
		return err
	}
	size, err := boundedQueryInt(c, "page_size", 25, 100)
	if err != nil {
		return err
	}
	raw, err := h.db.ListTrackingRecipients(ctx, database.ListTrackingRecipientsParams{TaskID: id, Filter: filter, PageLimit: int32(size), PageOffset: int32((page - 1) * size)})
	if err != nil {
		return err
	}
	result := TrackingRecipientPage{TrackingAvailable: h.mailer.TrackingAvailable(), Page: page, PageSize: size}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	return c.JSON(result)
}
