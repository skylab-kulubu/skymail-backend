package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/mail"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

const formSourceLifetime = 30 * time.Minute

var (
	errFormSourceUnavailable = apperrors.New("form_source.unavailable", "Forms source preparation is unavailable.", fiber.StatusServiceUnavailable)
	errFormSourceExpired     = apperrors.New("form_source.expired", "Prepare the Forms audience again.", fiber.StatusGone)
	errFormSourceChanged     = apperrors.New("form_source.changed", "The Forms snapshot is no longer available; prepare a new audience.", fiber.StatusConflict)
	errFormSourceConsumed    = apperrors.New("form_source.consumed", "The Forms snapshot already belongs to a send or approval.", fiber.StatusConflict)
	errFormSourceBusy        = apperrors.New("form_source.busy", "Another action is using this Forms snapshot.", fiber.StatusConflict)
)

type FormMailSource struct {
	ID             uuid.UUID `json:"id"`
	FormID         uuid.UUID `json:"form_id"`
	OwnerSub       string    `json:"owner_sub"`
	FormTitle      string    `json:"form_title"`
	RecipientCount int       `json:"recipient_count"`
	ResponseCount  int       `json:"response_count"`
	SkippedCount   int       `json:"skipped_count"`
	DuplicateCount int       `json:"duplicate_count"`
	PreparedAt     time.Time `json:"prepared_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func formSourceDescriptor(s database.MailFormSource) FormMailSource {
	return FormMailSource{ID: s.ID, FormID: s.FormID, OwnerSub: s.OwnerSub, FormTitle: s.FormTitle, RecipientCount: s.RecipientCount,
		ResponseCount: s.ResponseCount, SkippedCount: s.SkippedCount, DuplicateCount: s.DuplicateCount, PreparedAt: s.PreparedAt, ExpiresAt: s.ExpiresAt}
}

func WithFormSourceLimit(limit int) MailHandlerOption {
	return func(h *mailHandlerImpl) { h.formSourceLimit = limit }
}

// WithFormSourceClock uses the same clock as the approval handler in tests.
func WithFormSourceClock(now func() time.Time) MailHandlerOption {
	return func(h *mailHandlerImpl) {
		if now != nil {
			h.now = now
		}
	}
}

// Only the new source bodies reject unknown fields. In particular, a browser
// cannot turn a source send into a caller-supplied recipient or actor list.
func bindFormSourceBody(c fiber.Ctx, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return formSourceValidation("body", "invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return formSourceValidation("body", "invalid")
	}
	return validator.NewStructValidator().Validate(out)
}

func formSourceValidation(field, code string) error {
	return apperrors.ErrValidation.WithParams(map[string]interface{}{"errors": []validator.FieldError{{Field: field, Code: code}}})
}

func validateFormSource(p requests.CreateFormMailSource, limit int) ([]mailer.RecipientInfo, error) {
	if limit <= 0 {
		return nil, errFormSourceUnavailable
	}
	if err := validator.NewStructValidator().Validate(p); err != nil {
		return nil, err
	}
	if len(p.Recipients) > limit {
		return nil, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: "recipients", Code: "max_length", Params: map[string]interface{}{"limit": limit}}}})
	}
	if p.ResponseCount > math.MaxInt32 || p.SkippedCount > math.MaxInt32 || p.DuplicateCount > math.MaxInt32 ||
		int64(p.ResponseCount) != int64(len(p.Recipients))+int64(p.SkippedCount)+int64(p.DuplicateCount) {
		return nil, formSourceValidation("response_count", "mismatch")
	}
	to, err := recipientsOf(p.Recipients)
	if err != nil {
		return nil, err
	}
	for _, r := range to {
		address, err := mail.ParseAddress(r.Email)
		at := strings.LastIndexByte(r.Email, '@')
		if len(r.Email) > 254 || err != nil || address.Address != r.Email || at < 0 || !strings.Contains(r.Email[at+1:], ".") || strings.ContainsAny(r.Email, " \t\r\n") {
			return nil, formSourceValidation("recipients", "invalid_email")
		}
	}
	return to, nil
}

func formSnapshotHash(owner string, formID uuid.UUID, title string, to []mailer.RecipientInfo, response, skipped, duplicates int) []byte {
	data, _ := json.Marshal(struct {
		Owner                         string
		FormID                        uuid.UUID
		Title                         string
		Recipients                    []MailApprovalRecipient
		Response, Skipped, Duplicates int
	}{owner, formID, title, approvalRecipients(to), response, skipped, duplicates})
	sum := sha256.Sum256(data)
	return sum[:]
}

func formSourceRequestHash(kind string, template uuid.UUID, variables []byte, tracking bool) ([]byte, error) {
	var values map[string]interface{}
	if len(variables) > 0 {
		if err := json.Unmarshal(variables, &values); err != nil {
			return nil, err
		}
	}
	if values == nil {
		values = map[string]interface{}{}
	}
	data, err := json.Marshal(struct {
		Kind      string
		Template  uuid.UUID
		Variables map[string]interface{}
		Tracking  bool
	}{kind, template, values, tracking})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}

func formSourceLockError(err error) error {
	if isNotFound(err) {
		return apperrors.ErrStatusNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return errFormSourceBusy
	}
	return err
}

func formSourceRecipients(ctx context.Context, q *database.Queries, s database.MailFormSource) ([]mailer.RecipientInfo, error) {
	if s.InvalidatedAt != nil {
		return nil, errFormSourceChanged
	}
	rows, err := q.ListMailFormSourceRecipients(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	to := make([]mailer.RecipientInfo, len(rows))
	for i, r := range rows {
		to[i] = mailer.RecipientInfo{FullName: r.FullName, Email: r.Email}
	}
	hash := formSnapshotHash(s.OwnerSub, s.FormID, s.FormTitle, to, s.ResponseCount, s.SkippedCount, s.DuplicateCount)
	if len(to) != s.RecipientCount || !bytes.Equal(hash, s.SnapshotHash) {
		return nil, errFormSourceChanged
	}
	return to, nil
}

// An exact retry returns the committed target before the preparation TTL is
// considered. It never creates another target, even after an approval edit.
func formSourceRetry(s database.MailFormSource, kind string, hash []byte, now time.Time) (*uuid.UUID, error) {
	if s.InvalidatedAt != nil {
		return nil, errFormSourceChanged
	}
	if s.ConsumedAt != nil {
		target := s.ConsumedApprovalID
		if kind == "direct" {
			target = s.ConsumedTaskID
		}
		if target != nil && bytes.Equal(hash, s.ConsumeRequestHash) {
			return target, nil
		}
		return nil, errFormSourceConsumed
	}
	if !now.Before(s.ExpiresAt) {
		return nil, errFormSourceExpired
	}
	return nil, nil
}

func checkFormSourceTemplate(ctx context.Context, q *database.Queries, id uuid.UUID, variables []byte, to mailer.RecipientInfo) (checkedSend, error) {
	template, err := q.ShareLockTemplate(ctx, id)
	if isNotFound(err) || err == nil && (template.ArchivedAt != nil || template.PublishedVersionID == nil) {
		return checkedSend{}, errApprovalTemplateUnavailable
	}
	if err != nil {
		return checkedSend{}, err
	}
	version, err := q.GetTemplateVersion(ctx, database.GetTemplateVersionParams{TemplateID: id, ID: *template.PublishedVersionID})
	if err != nil {
		return checkedSend{}, err
	}
	if err := checkApprovalValues(template, version, variables, to); err != nil {
		return checkedSend{}, err
	}
	return checkedSend{template: template, versionID: *template.PublishedVersionID}, nil
}

func (h *mailHandlerImpl) CreateFormSource(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	var p requests.CreateFormMailSource
	if err := bindFormSourceBody(c, &p); err != nil {
		return err
	}
	to, err := validateFormSource(p, h.formSourceLimit)
	if err != nil {
		return err
	}
	now := h.now().UTC()
	var source database.MailFormSource
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		var err error
		source, err = q.CreateMailFormSource(c.Context(), database.CreateMailFormSourceParams{
			ID: uuid.New(), OwnerSub: p.OwnerSub.String(), FormID: p.FormID, FormTitle: p.FormTitle, PreparedAt: now, ExpiresAt: now.Add(formSourceLifetime),
			ResponseCount: p.ResponseCount, RecipientCount: len(to), SkippedCount: p.SkippedCount, DuplicateCount: p.DuplicateCount,
			SnapshotHash: formSnapshotHash(p.OwnerSub.String(), p.FormID, p.FormTitle, to, p.ResponseCount, p.SkippedCount, p.DuplicateCount),
		})
		if err != nil {
			return err
		}
		emails, names := make([]string, len(to)), make([]string, len(to))
		for i, r := range to {
			emails[i], names[i] = r.Email, r.FullName
		}
		return duplicateRecipientsError(q.AddMailFormSourceRecipients(c.Context(), database.AddMailFormSourceRecipientsParams{SourceID: source.ID, Emails: emails, FullNames: names}))
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(formSourceDescriptor(source))
}

func (h *mailHandlerImpl) GetFormSource(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	owner, _ := c.Locals("user_id").(string)
	source, err := h.db.GetOwnedMailFormSource(c.Context(), database.GetOwnedMailFormSourceParams{ID: id, OwnerSub: owner})
	if err != nil {
		return formSourceLockError(err)
	}
	if source.InvalidatedAt != nil {
		return errFormSourceChanged
	}
	if source.ConsumedAt != nil {
		return errFormSourceConsumed
	}
	if !h.now().Before(source.ExpiresAt) {
		return errFormSourceExpired
	}
	return c.JSON(formSourceDescriptor(source))
}

func (h *mailHandlerImpl) SendFormSource(c fiber.Ctx) error {
	var p requests.SendFormSource
	if err := bindFormSourceBody(c, &p); err != nil {
		return err
	}
	m, ok := h.mailer.(mailer.Transactional)
	if !ok {
		return errFormSourceUnavailable
	}
	owner, _ := c.Locals("user_id").(string)
	if owner == "" {
		return apperrors.ErrForbidden
	}
	variables, err := json.Marshal(p.BodyVariables)
	if err != nil {
		return err
	}
	if p.BodyVariables == nil {
		variables = []byte(`{}`)
	}
	hash, err := formSourceRequestHash("direct", p.TemplateID, variables, p.Tracking)
	if err != nil {
		return err
	}
	var id uuid.UUID
	repeated := false
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		source, err := q.LockOwnedMailFormSource(c.Context(), database.LockOwnedMailFormSourceParams{ID: p.FormSourceID, OwnerSub: owner})
		if err != nil {
			return formSourceLockError(err)
		}
		retry, err := formSourceRetry(source, "direct", hash, h.now())
		if err != nil {
			return err
		}
		if retry != nil {
			id = *retry
			repeated = true
			return nil
		}
		to, err := formSourceRecipients(c.Context(), q, source)
		if err != nil {
			return err
		}
		if _, err = checkFormSourceTemplate(c.Context(), q, p.TemplateID, variables, to[0]); err != nil {
			return err
		}
		id, err = m.Queue(q, mailer.PriorityBulk).EnqueueWithRecipients(c.Context(), mailer.EnqueueWithRecipientsParams{
			SentBy: owner, TemplateID: p.TemplateID, FormSourceID: &source.ID, BodyVariables: variables, Recipients: to,
		})
		if err != nil {
			return err
		}
		consumedAt := h.now()
		_, err = q.ConsumeMailFormSource(c.Context(), database.ConsumeMailFormSourceParams{ID: source.ID, At: &consumedAt, TaskID: &id, RequestHash: hash})
		return err
	})
	if err != nil {
		return err
	}
	status := fiber.StatusCreated
	if repeated {
		status = fiber.StatusOK
	} else {
		m.Wake()
	}
	return c.Status(status).JSON(CreatedMailTask{ID: id, Tracking: false})
}

func (h *mailApprovalHandlerImpl) submitFormSource(c fiber.Ctx, caller approvalCaller, p requests.SubmitMailApproval, send approvalSend) error {
	hash, err := formSourceRequestHash("approval", p.TemplateID, send.variables, p.Tracking)
	if err != nil {
		return err
	}
	var id uuid.UUID
	repeated := false
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		source, err := q.LockOwnedMailFormSource(c.Context(), database.LockOwnedMailFormSourceParams{ID: *p.FormSourceID, OwnerSub: caller.sub})
		if err != nil {
			return formSourceLockError(err)
		}
		retry, err := formSourceRetry(source, "approval", hash, h.now())
		if err != nil {
			return err
		}
		if retry != nil {
			id = *retry
			repeated = true
			return nil
		}
		to, err := formSourceRecipients(c.Context(), q, source)
		if err != nil {
			return err
		}
		checked, err := checkFormSourceTemplate(c.Context(), q, p.TemplateID, send.variables, to[0])
		if err != nil {
			return err
		}
		now := h.now()
		a, err := q.CreateMailApproval(c.Context(), database.CreateMailApprovalParams{
			SubmitterSub: caller.sub, SubmitterName: caller.name, SubmitterEmail: caller.email, SubmitterEmailUnverified: caller.emailUnverified,
			TemplateID: p.TemplateID, TemplateVersionID: checked.versionID, FormSourceID: &source.ID, BodyVariables: send.variables,
			At: now, DeadlineAt: now.Add(mailApprovalDeadline),
		})
		if err != nil {
			return err
		}
		id = a.ID
		if _, err = q.ConsumeMailFormSource(c.Context(), database.ConsumeMailFormSourceParams{ID: source.ID, At: &now, ApprovalID: &id, RequestHash: hash}); err != nil {
			return err
		}
		_, err = q.RecordMailApprovalEvent(c.Context(), database.RecordMailApprovalEventParams{ApprovalID: id, Kind: database.MailApprovalEventKindSubmitted, ActorSub: &caller.sub, ActorName: caller.name, At: now})
		return err
	})
	if err != nil {
		return err
	}
	view, err := h.db.GetMailApproval(c.Context(), id)
	if err != nil {
		return err
	}
	var notification *MailApprovalNotification
	if !repeated {
		notification = h.deliver(c.Context(), view, approvalNotice{kind: noticeRequested, by: caller.sub})
	}
	answer, err := h.approval(c.Context(), view)
	if err != nil {
		return err
	}
	answer.Notification = notification
	status := fiber.StatusCreated
	if repeated {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(answer)
}

func queueToFormSource(ctx context.Context, q *database.Queries, sends mailer.Queue, a database.MailApproval) (uuid.UUID, error) {
	source, err := q.LockMailFormSource(ctx, *a.FormSourceID)
	if err != nil {
		return uuid.Nil, formSourceLockError(err)
	}
	if source.ConsumedApprovalID == nil || *source.ConsumedApprovalID != a.ID || source.OwnerSub != a.SubmitterSub {
		return uuid.Nil, errFormSourceChanged
	}
	to, err := formSourceRecipients(ctx, q, source)
	if err != nil {
		return uuid.Nil, err
	}
	return sends.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{SentBy: a.SubmitterSub, TemplateID: a.TemplateID, FormSourceID: a.FormSourceID, BodyVariables: a.BodyVariables, Recipients: to})
}
