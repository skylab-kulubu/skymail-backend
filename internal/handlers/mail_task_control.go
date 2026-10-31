package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// A send's controls (hub ticket duyuru-haber 05, G4; README, "Kısma ve görev
// düzeyinde işlemler"): pause, resume and cancel. Only a bulk send has them:
// its rows wait in the bulk lane, while a single mail is sent at once. Who
// may send to everyone (skymail:announcements:send) controls any send; who
// may send to a list (skymail:mails:write) controls their own.

var (
	errMailTaskNotBulk = apperrors.New("mail_task.not_bulk",
		"Only a send to a list, a group, people or a News send can be paused, resumed or cancelled: a single mail is sent at once.",
		fiber.StatusConflict)
	errMailTaskCancelled = apperrors.New("mail_task.cancelled",
		"The send was cancelled: it cannot be paused or resumed.",
		fiber.StatusConflict)
	errMailTaskFinished = apperrors.New("mail_task.finished",
		"Every recipient of the send has been handled: nothing is left to pause or cancel.",
		fiber.StatusConflict)
)

type taskControl int

const (
	pauseTask taskControl = iota
	resumeTask
	cancelTask
)

// PauseTask godoc
//
//	@Summary		Pause a send
//	@Description	The bulk lane takes none of the send's rows until it is resumed; rows being sent finish. A paused send stays paused (200). The send's own sender with skymail:mails:write, or anyone with skymail:announcements:send. A send may also pause itself when its recipients fail past SKYMAIL_FAILURE_PAUSE_COUNT and SKYMAIL_FAILURE_PAUSE_PERCENT (paused_reason failure_rate).
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	handlers.MailTaskItem
//	@Failure		403	{object}	apperrors.AppError	"Forbidden: neither skymail:announcements:send nor the send's sender"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		409	{object}	apperrors.AppError	"mail_task.not_bulk, mail_task.cancelled or mail_task.finished"
//	@Router			/mail_tasks/{id}/pause [post]
func (h *mailHandlerImpl) PauseTask(c fiber.Ctx) error { return h.controlTask(c, pauseTask) }

// ResumeTask godoc
//
//	@Summary		Resume a paused send
//	@Description	The bulk lane takes the send's rows again; its failures so far are accepted and its failure rate counts afresh. A send that is not paused is left as it is (200). Who may pause it may resume it.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	handlers.MailTaskItem
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		409	{object}	apperrors.AppError	"mail_task.not_bulk or mail_task.cancelled"
//	@Router			/mail_tasks/{id}/resume [post]
func (h *mailHandlerImpl) ResumeTask(c fiber.Ctx) error { return h.controlTask(c, resumeTask) }

// CancelTask godoc
//
//	@Summary		Cancel a send
//	@Description	The send's pending recipients are written failed with 'cancelled', nothing tried (counted in recipient_counts.cancelled, never making the send read failed); rows being sent finish. A cancelled send stays cancelled (200). Who may pause it may cancel it.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	handlers.MailTaskItem
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		409	{object}	apperrors.AppError	"mail_task.not_bulk or mail_task.finished"
//	@Router			/mail_tasks/{id}/cancel [post]
func (h *mailHandlerImpl) CancelTask(c fiber.Ctx) error { return h.controlTask(c, cancelTask) }

func (h *mailHandlerImpl) controlTask(c fiber.Ctx, control taskControl) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}
	caller := contentCallerOf(c)
	if caller.sub == "" {
		return apperrors.ErrForbidden
	}
	ctx := c.Context()
	err = h.db.InTx(ctx, func(q *database.Queries) error {
		task, err := q.GetMailTaskControl(ctx, id)
		if err != nil {
			return err
		}
		if !caller.has(AnnouncementsSendRole) && task.SentBy != caller.sub {
			return apperrors.ErrForbidden
		}
		if !task.Bulk {
			return errMailTaskNotBulk
		}
		switch control {
		case pauseTask:
			if task.CancelledAt != nil {
				return errMailTaskCancelled
			}
			if task.PausedAt != nil {
				return nil
			}
			if !task.Unfinished {
				return errMailTaskFinished
			}
			_, err = q.PauseMailTask(ctx, database.PauseMailTaskParams{ID: id, PausedBy: caller.sub})
		case resumeTask:
			if task.CancelledAt != nil {
				return errMailTaskCancelled
			}
			_, err = q.ResumeMailTask(ctx, id)
		case cancelTask:
			if task.CancelledAt != nil {
				return nil
			}
			if !task.Unfinished {
				return errMailTaskFinished
			}
			if _, err = q.CancelMailTask(ctx, database.CancelMailTaskParams{ID: id, CancelledBy: caller.sub}); err == nil {
				_, err = q.CancelPendingRows(ctx, &id)
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	sends, err := h.db.ListMailTaskSends(ctx, database.ListMailTaskSendsParams{TaskID: &id, Limit: 1})
	if err != nil {
		return err
	}
	if len(sends) == 0 {
		return pgx.ErrNoRows
	}
	return c.JSON(h.mailTaskItems(ctx, caller, sends)[0])
}
