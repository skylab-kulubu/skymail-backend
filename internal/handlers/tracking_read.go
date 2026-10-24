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

// TrackingSummary has no identities or target URLs. 90 days after the last
// mail went out (tracking.EventRetention) report is the retained aggregate,
// or null when there is none (a send of fewer than 10 tracked recipients a
// year after it was created). Withheld
// says the send's counts would tell of people (see GetTracking) and the
// caller may not read its people: report is then null.
type TrackingSummary struct {
	TrackingAvailable bool               `json:"tracking_available"`
	TrackingRequested bool               `json:"tracking_requested"`
	Retained          bool               `json:"retained"`
	Withheld          bool               `json:"withheld"`
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
//	@Description	Requires skymail:mails:read. No identities, link tokens or target URLs. After 90 days only retained totals, or null: for good with at least 10 tracked recipients, until a year after the send with fewer. tracking_available follows the current flag. A send's counts tell of people when it has fewer than 10 tracked recipients (tracking.TotalsMinRecipients) or is mail the system sends (the system lane): then only one who may read its people (GET /mail_tasks/{id}/tracking/recipients: skymail:mails:tracking:read, or the send's own sender outside the system lane) gets report; anyone else gets withheld true and report null (KVKK notice s3, §4).
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
	withheld := aggregate != nil && countsTellOfPeople(task, aggregate) && !mayReadTrackingPeople(c, task)
	if withheld {
		aggregate = nil
	}
	return c.JSON(TrackingSummary{TrackingAvailable: h.mailer.TrackingAvailable(), TrackingRequested: task.TrackingRequested, Retained: retained, Withheld: withheld, Report: aggregate})
}

// countsTellOfPeople says a send's counts are as good as its people's: a
// send to fewer than tracking.TotalsMinRecipients tracked people ("1 of 1
// opened at 14:00" names the one whose address GET /:id/queue shows), or
// mail the system sends, whose people only YK, DK and the admins see.
func countsTellOfPeople(task database.TrackingTaskAccessRow, report *TrackingAggregate) bool {
	return task.SystemLane || report.TrackedRecipients < tracking.TotalsMinRecipients
}

// mayReadTrackingPeople is who reads a send's people: skymail:mails:tracking:read,
// or the send's own sender. A service is no sender here: the mail it sends
// (SKYMAIL_TRACKING_SYSTEM) is read with the role only. Nor is the person a
// system mail names as its sender (a Mail onayı notice's requester or
// decider): the system lane's mail is the system's, whose people only YK,
// DK and the admins see (KVKK notice s3, §4).
func mayReadTrackingPeople(c fiber.Ctx, task database.TrackingTaskAccessRow) bool {
	caller := contentCallerOf(c)
	sender := caller.sub != "" && caller.sub == task.SentBy && !middlewares.IsServiceAccount(c) && !task.SystemLane
	return caller.has(TrackingReadRole) || sender
}

// GetTrackingRecipients godoc
//
//	@Summary		Recipient opening and click observations
//	@Description	Requires skymail:mails:tracking:read or the send's own sender (sent_by equals token sub, a person's token: a service's token reads the mail it sent only with the role). Mail the system sends (account and transaction mail in the system lane, SkyMail's own Mail onayı notices included, though sent_by names the person who acted) is read with the role only: its sender is refused. First likely human opening falls back to a non-automated click, marked click_inferred. Returns 410 90 days after the last mail went out, even if retention has not run. Does not return tracking tokens or target URLs.
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
	if !mayReadTrackingPeople(c, task) {
		return apperrors.ErrForbidden
	}
	if task.TrackingClosedAt != nil || !task.SentAt.Add(tracking.EventRetention).After(h.now()) {
		return apperrors.New("mail.tracking_expired", "Kişi bazında takip kaydı 90 gün sonra silindi.", fiber.StatusGone)
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
