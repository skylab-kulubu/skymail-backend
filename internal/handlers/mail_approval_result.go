package handlers

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
)

// MailApprovalResult is how an approved request's sends went, in totals only:
// its submitter reads it without skymail:mails:read, which the send's own
// screens take (hub ticket ekip-postasi 09). No person is in it, and a request
// to people, one send each, has one total, never a row per send: a row each
// would tell each person's delivery. Opens and clicks are summed by the rule
// GET /mail_tasks/{id}/tracking reads a send's by (ADR-0074 decision 9,
// addendum 2026-11-08): withheld below 10 tracked recipients and in the
// system lane from one who may not read the sends' people. tracking, those
// opens and clicks, is null when none was tracked, when tracking_withheld,
// when no totals are kept (a year after a send to fewer than 10), or when they
// could not be read in time (tracking_withheld false then); it has no
// comment of its own, which would become its type's description in the
// OpenAPI document.
type MailApprovalResult struct {
	// Recipients it went out to (one a delivery report later said does not exist included).
	Sent int64 `json:"sent"`
	// Recipients never sent to, by rule: a suppressed or unsubscribed address, a category the person turned off, an account disabled or being erased, an address two accounts hold, a reserved domain, a cancelled send.
	Skipped int64 `json:"skipped"`
	// Recipients the mail was tried for and failed.
	Failed int64 `json:"failed"`
	// Recipients still queued or being sent.
	Pending  int64                 `json:"pending"`
	Tracking *MailApprovalTracking `json:"tracking"`
	// Its sends were tracked, but their counts tell of people (fewer than 10 tracked recipients, or the system lane) and the caller may not read the sends' people: tracking is null.
	TrackingWithheld bool `json:"tracking_withheld"`
}

// MailApprovalTracking is an approved request's opens and clicks, summed over
// its tracked sends, without a person or a link in it.
type MailApprovalTracking struct {
	// Recipients tracked.
	TrackedRecipients int `json:"tracked_recipients"`
	// Recipients with an open or a click not marked automatic (unique_likely_human_opens).
	Opened int `json:"opened"`
	// Recipients who clicked a link (unique_clickers).
	Clicked int `json:"clicked"`
}

// trackedSend is one of a request's tracked sends and its counts as GET
// /mail_tasks/{id}/tracking reads them; report is nil when none are kept.
type trackedSend struct {
	task   database.TrackingTaskAccessRow
	report *TrackingAggregate
}

// result is how the approved request's sends went (MailApprovalResult), for
// the caller of c. It never fails the read of the request: when the delivery
// totals cannot be read in time it is nil (no result), and when the opens and
// clicks cannot, tracking is null with tracking_withheld false. Each part has
// trackingReadBudget of its own; either failure is logged.
func (h *mailApprovalHandlerImpl) result(c fiber.Ctx, approvalID uuid.UUID, taskIDs []uuid.UUID) *MailApprovalResult {
	ctx, cancel := context.WithTimeout(c.Context(), trackingReadBudget)
	counts, err := h.db.CountMailTasksOutcomes(ctx, taskIDs)
	cancel()
	if err != nil {
		log.Warn().Err(err).Str("approval_id", approvalID.String()).Msg("could not count an approved request's sends; read without result")
		return nil
	}
	result := &MailApprovalResult{Sent: counts.Sent, Skipped: counts.Skipped, Failed: counts.Failed, Pending: counts.Pending}
	sends, err := h.trackedSends(c.Context(), taskIDs)
	if err != nil {
		log.Warn().Err(err).Str("approval_id", approvalID.String()).Msg("could not read an approved request's opens and clicks; result without tracking")
		return result
	}
	result.Tracking, result.TrackingWithheld = approvalTracking(contentCallerOf(c), middlewares.IsServiceAccount(c), sends, h.senderSeesRecipients)
	return result
}

// trackedSends is the request's tracked sends and each one's counts as GET
// /mail_tasks/{id}/tracking reads them, within trackingReadBudget: who sent
// them and when in one query, then a report per tracked send.
func (h *mailApprovalHandlerImpl) trackedSends(ctx context.Context, taskIDs []uuid.UUID) ([]trackedSend, error) {
	ctx, cancel := context.WithTimeout(ctx, trackingReadBudget)
	defer cancel()
	tasks, err := h.db.TrackingTasksAccess(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	var sends []trackedSend
	for _, row := range tasks {
		task := database.TrackingTaskAccessRow(row)
		if !task.TrackingRequested {
			continue
		}
		report, _, err := trackingReportOf(ctx, h.db, task, h.now())
		if err != nil {
			return nil, err
		}
		sends = append(sends, trackedSend{task: task, report: report})
	}
	return sends, nil
}

// approvalTracking sums a request's tracked sends' opens and clicks, and says
// whether they are withheld from the caller: by countsTellOfPeople over the
// sum, and over the sends whose own counts tell of people when others' do
// not — one who reads the others' (GET /mail_tasks/{id}/tracking) would
// subtract them from the sum — unless the caller may read every send's people
// (trackingPeopleReadable). A send whose totals are gone leaves no sum.
func approvalTracking(caller contentCaller, service bool, sends []trackedSend, senderSees bool) (*MailApprovalTracking, bool) {
	if len(sends) == 0 {
		return nil, false
	}
	var all, small TrackingAggregate
	var allTask, smallTask database.TrackingTaskAccessRow
	smallSends := 0
	people := true
	for _, send := range sends {
		if send.report == nil {
			return nil, false
		}
		people = people && trackingPeopleReadable(caller, service, send.task, senderSees)
		addTracking(&all, &allTask, send)
		if countsTellOfPeople(send.task, send.report) {
			addTracking(&small, &smallTask, send)
			smallSends++
		}
	}
	tells := countsTellOfPeople(allTask, &all) ||
		(smallSends > 0 && smallSends < len(sends) && countsTellOfPeople(smallTask, &small))
	if tells && !people {
		return nil, true
	}
	return &MailApprovalTracking{TrackedRecipients: all.TrackedRecipients, Opened: all.UniqueLikelyHumanOpens, Clicked: all.UniqueClickers}, false
}

func addTracking(sum *TrackingAggregate, task *database.TrackingTaskAccessRow, send trackedSend) {
	sum.TrackedRecipients += send.report.TrackedRecipients
	sum.UniqueLikelyHumanOpens += send.report.UniqueLikelyHumanOpens
	sum.UniqueClickers += send.report.UniqueClickers
	task.SystemLane = task.SystemLane || send.task.SystemLane
}
