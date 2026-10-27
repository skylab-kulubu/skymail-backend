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
	// The prepared source ID, distinct from form_id; consumed by one owner send or approval.
	ID        uuid.UUID `json:"id"`
	FormID    uuid.UUID `json:"form_id"`
	OwnerSub  string    `json:"owner_sub"`
	FormTitle string    `json:"form_title"`
	// Fixed valid, deduplicated recipients; addresses are never returned here.
	RecipientCount int `json:"recipient_count"`
	ResponseCount  int `json:"response_count"`
	SkippedCount   int `json:"skipped_count"`
	DuplicateCount int `json:"duplicate_count"`
	// Of RecipientCount, those whose name is empty: a mail that greets its
	// recipient by name (the recipient name tag) prints the tag's fallback,
	// or nothing, for them. The send page says how many.
	UnnamedCount int `json:"unnamed_count"`
	// Snapshot preparation time, not the time the owner later sends or submits it.
	PreparedAt time.Time `json:"prepared_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func formSourceDescriptor(s database.MailFormSource, unnamed int) FormMailSource {
	return FormMailSource{ID: s.ID, FormID: s.FormID, OwnerSub: s.OwnerSub, FormTitle: s.FormTitle, RecipientCount: s.RecipientCount,
		ResponseCount: s.ResponseCount, SkippedCount: s.SkippedCount, DuplicateCount: s.DuplicateCount, UnnamedCount: unnamed,
		PreparedAt: s.PreparedAt, ExpiresAt: s.ExpiresAt}
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

// formSourceRequestHash is what an exact retry of a Forms source's send or
// request repeats. purpose is the purpose its body named (choosePurpose), not
// the default a body naming none gets: none hashes as before purposes, so a
// retry across the release, or across turning them on, is still the same
// request.
func formSourceRequestHash(kind string, template uuid.UUID, variables []byte, tracking bool, purpose mailer.SendPurpose) ([]byte, error) {
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
		Kind                    string
		Template                uuid.UUID
		Variables               map[string]interface{}
		Tracking                bool
		Purpose                 *string `json:",omitempty"`
		Category                *string `json:",omitempty"`
		TransactionalStatement  *bool   `json:",omitempty"`
		CommercialContentAbsent *bool   `json:",omitempty"`
	}{kind, template, values, tracking, purpose.Purpose, purpose.Category, purpose.TransactionalStatement, purpose.CommercialContentAbsent})
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
	to := formSourceRecipientInfos(rows)
	hash := formSnapshotHash(s.OwnerSub, s.FormID, s.FormTitle, to, s.ResponseCount, s.SkippedCount, s.DuplicateCount)
	if len(to) != s.RecipientCount || !bytes.Equal(hash, s.SnapshotHash) {
		return nil, errFormSourceChanged
	}
	return to, nil
}

func formSourceRecipientInfos(rows []database.MailFormSourceRecipient) []mailer.RecipientInfo {
	to := make([]mailer.RecipientInfo, len(rows))
	for i, r := range rows {
		to[i] = mailer.RecipientInfo{FullName: r.FullName, Email: r.Email}
	}
	return to
}

// formSourceTracking is, while SkyMail tracks every person's send
// (SKYMAIL_TRACKING_OPERATOR), whom of a Forms source's respondents the
// send may track, asked before the transaction that locks the source: each
// address looked up in Keycloak (exact verified primary e-mail; none is
// no_account), all of them at once within the people send's budget, then
// core. The source is immutable, so the people asked about are the people
// the send goes to. Nil when the flag is off, or the source cannot be read
// (the transaction then refuses it, or tracks no one), or — direct, a send
// that consumes the source — it is consumed already: the send is a retry,
// answered with the send it made, and no one is asked again.
func formSourceTracking(ctx context.Context, db *database.Store, m mailer.Transactional, sourceID uuid.UUID, owner string, templateID uuid.UUID, direct bool) *mailer.TrackingPreferences {
	if !m.OperatorTrackingAlways() {
		return nil
	}
	source, err := db.GetOwnedMailFormSource(ctx, database.GetOwnedMailFormSourceParams{ID: sourceID, OwnerSub: owner})
	if err != nil || source.InvalidatedAt != nil || (direct && source.ConsumedAt != nil) {
		return nil
	}
	rows, err := db.ListMailFormSourceRecipients(ctx, source.ID)
	if err != nil {
		return nil
	}
	return m.ResolvePeopleTracking(ctx, templateID, formSourceRecipientInfos(rows))
}

// formSourcePreferences is what core says, while the bulk preference check
// is on, about a Forms source's respondents for a duyuru send of purpose,
// asked before the transaction that locks the source (mailer,
// preference_gate.go). The source is immutable, so the people asked about
// are the people the send goes to. Nil when the check is off, the send is
// not duyuru, the source cannot be read (the transaction then refuses it)
// or — direct — it is consumed already: the send is a retry, answered with
// the send it made, and core is not asked again.
func formSourcePreferences(ctx context.Context, db *database.Store, m mailer.Transactional, sourceID uuid.UUID, owner string, purpose mailer.SendPurpose, direct bool) (*mailer.PreferenceVerdicts, error) {
	if !m.PreferenceAvailable() || !isDuyuru(purpose) {
		return nil, nil
	}
	source, err := db.GetOwnedMailFormSource(ctx, database.GetOwnedMailFormSourceParams{ID: sourceID, OwnerSub: owner})
	if err != nil || source.InvalidatedAt != nil || (direct && source.ConsumedAt != nil) {
		return nil, nil
	}
	rows, err := db.ListMailFormSourceRecipients(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	return m.ResolvePreferences(ctx, purpose, formSourceRecipientInfos(rows))
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

// CreateFormSource godoc
//
//	@Summary		Prepare an immutable Forms audience
//	@Description	Only the configured Forms service account may create this source, after the existing bearer userinfo, SkyMail access, send-or-write permission and exact service identity checks. Forms derives owner_sub, form metadata and recipients from its authorized HTTP caller. No mail or approval is created. The source expires after 30 minutes unless its owner consumes it. The configured recipient count and existing HTTP body limits reject excess input without truncation. The response is metadata only, with no recipient addresses, and is not cacheable.
//	@Tags			Mail
//	@Param			source	body	requests.CreateFormMailSource	true	"Authorized Forms snapshot"
//	@Accept			json
//	@Produce		json
//	@Success		201	{object}	handlers.FormMailSource
//	@Failure		400	{object}	apperrors.AppError	"Invalid snapshot, duplicate address or configured count limit exceeded"
//	@Failure		403	{object}	apperrors.AppError	"The caller is not the configured Forms service or lacks current send-or-write permission"
//	@Failure		413	{string}	string				"Existing request body limit exceeded"
//	@Failure		503	{object}	apperrors.AppError	"Forms source preparation is not configured"
//	@Router			/form_mail_sources [post]
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
	unnamed := 0
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
		unnamed = 0
		for i, r := range to {
			emails[i], names[i] = r.Email, r.FullName
			if strings.TrimSpace(r.FullName) == "" {
				unnamed++
			}
		}
		return duplicateRecipientsError(q.AddMailFormSourceRecipients(c.Context(), database.AddMailFormSourceRecipientsParams{SourceID: source.ID, Emails: emails, FullNames: names}))
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(formSourceDescriptor(source, unnamed))
}

// GetFormSource godoc
//
//	@Summary		Read a prepared Forms audience
//	@Description	Only the authenticated owner can read this metadata descriptor; approver or admin roles do not override ownership. Reading never consumes the source and never returns recipient addresses. The response is not cacheable. Historical send and approval audience metadata is read through those existing APIs instead.
//	@Tags			Mail
//	@Produce		json
//	@Param			id	path		string	true	"Prepared source ID, distinct from the form ID"
//	@Success		200	{object}	handlers.FormMailSource
//	@Failure		404	{object}	apperrors.AppError	"No such source owned by the caller"
//	@Failure		409	{object}	apperrors.AppError	"Source invalidated or already consumed"
//	@Failure		410	{object}	apperrors.AppError	"Prepare the Forms audience again"
//	@Router			/form_mail_sources/{id} [get]
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
	unnamed, err := h.db.CountUnnamedMailFormSourceRecipients(c.Context(), source.ID)
	if err != nil {
		return err
	}
	return c.JSON(formSourceDescriptor(source, int(unnamed)))
}

// SendFormSource godoc
//
//	@Summary		Send to an immutable Forms audience
//	@Description	The source owner holding current skymail:mails:send or skymail:mails:write permission queues one bulk task for the fixed source recipients. Source consumption, task and queue rows commit together. The browser supplies no recipients or form metadata. An exact retry of the same source, template, canonical body variables and requested tracking returns the same task, including after preparation TTL; changed payload or target is refused. The send is untracked, and the answer says tracking false, unless SkyMail tracks every person's send (SKYMAIL_TRACKING_OPERATOR; summary tracking_always): then it is tracked whatever it asks, each respondent's account looked up in Keycloak by address before the transaction (none is no_account) and core deciding, and the answer says tracking true. Purposes (SKYMAIL_MAIL_PURPOSE; tracking-mode purpose_available): while they are on the send is written with purpose and category (README, "Gönderim amacı ve kategori"): duyuru with a category by default; islem with transactional_statement true when the mail is about each respondent's own application: tracked as any Forms source send, queued in the bulk lane, sent from bilgi@ without List-Unsubscribe. The purpose is part of an exact retry.
//	@Tags			Mail
//	@Param			send	body	requests.SendFormSource	true	"Prepared source and mail content"
//	@Accept			json
//	@Produce		json
//	@Success		201	{object}	handlers.CreatedMailTask	"New task"
//	@Success		200	{object}	handlers.CreatedMailTask	"Same task for an exact retry"
//	@Failure		400	{object}	apperrors.AppError			"Invalid request or caller-supplied recipient/metadata fields. Purpose, checked in this order, while purposes are on (SKYMAIL_MAIL_PURPOSE): mail_task.purpose_required, mail_task.commercial_content_not_allowed, mail_task.purpose_invalid, then the purpose's own: mail_task.category_not_allowed, mail_task.transactional_statement_required (islem); mail_task.category_required, mail_task.category_invalid, mail_task.commercial_statement_required (duyuru, SKYMAIL_BULK_PREFERENCE); mail_task.category_unknown: core calls the team category no team. A retry whose purpose differs is not the same request: 409 form_source.consumed"
//	@Failure		403	{object}	apperrors.AppError			"Current send-or-write permission required"
//	@Failure		404	{object}	apperrors.AppError			"No such source owned by the caller"
//	@Failure		409	{object}	apperrors.AppError			"Source invalidated, consumed differently or busy"
//	@Failure		410	{object}	apperrors.AppError			"Prepare the Forms audience again"
//	@Failure		422	{object}	apperrors.AppError			"Template or variables unavailable or invalid"
//	@Failure		503	{object}	apperrors.AppError			"Transactional sending unavailable; mail_task.preference_unavailable (Retry-After): a duyuru send while every duyuru send obeys the recipients' preferences (SKYMAIL_BULK_PREFERENCE), and core did not answer; nothing is queued, the source is not consumed"
//	@Router			/mail_tasks/form-source [post]
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
	// A Forms source's respondents get duyuru unless the sender declares the
	// mail about their own application (islem); either is tracked as before.
	purposesOn := m.PurposeAvailable()
	named, err := choosePurpose(purposesOn, m.PromotionalAvailable(), m.ListConsentAvailable(), m.PreferenceAvailable(), p.SendPurpose, targetFormSource)
	if err != nil {
		return err
	}
	if err := refuseAccountPromotional(c.Context(), h.db, p.TemplateID, named); err != nil {
		return err
	}
	purpose := purposeOrDefault(purposesOn, named, duyuruByDefault)
	hash, err := formSourceRequestHash("direct", p.TemplateID, variables, p.Tracking, named)
	if err != nil {
		return err
	}
	tracked := m.OperatorTrackingAlways()
	preferences := formSourceTracking(c.Context(), h.db, m, p.FormSourceID, owner, p.TemplateID, true)
	verdicts, err := formSourcePreferences(c.Context(), h.db, m, p.FormSourceID, owner, purpose, true)
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
		id, err = m.Queue(q, mailer.PriorityBulk).WithPreferences(verdicts).EnqueueWithRecipients(c.Context(), mailer.EnqueueWithRecipientsParams{
			SentBy: owner, TemplateID: p.TemplateID, FormSourceID: &source.ID, BodyVariables: variables, Recipients: to,
			Tracking: tracked, Preferences: preferences, Purpose: purpose,
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
		// The send the retry returns is as tracked, and has the purpose, it
		// was queued with.
		task, err := h.db.GetMailTaskById(c.Context(), id)
		if err != nil {
			return err
		}
		tracked = task.TrackingRequested
		purpose = mailer.SendPurpose{Purpose: task.Purpose, Category: task.Category}
	} else {
		m.Wake()
	}
	return c.Status(status).JSON(createdMailTask(id, tracked, purpose))
}

func (h *mailApprovalHandlerImpl) submitFormSource(c fiber.Ctx, caller approvalCaller, p requests.SubmitMailApproval, send approvalSend) error {
	// Checked already (approvalPurpose): what the body named, for the hash.
	named, err := choosePurpose(h.mailer.PurposeAvailable(), h.mailer.PromotionalAvailable(), h.mailer.ListConsentAvailable(), h.mailer.PreferenceAvailable(), send.askedPurpose, targetFormSource)
	if err != nil {
		return err
	}
	hash, err := formSourceRequestHash("approval", p.TemplateID, send.variables, p.Tracking, named)
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
		// A Forms source's send is tracked only while SkyMail tracks every
		// person's send: it has no box.
		a, err := q.CreateMailApproval(c.Context(), database.CreateMailApprovalParams{
			SubmitterSub: caller.sub, SubmitterName: caller.name, SubmitterEmail: caller.email, SubmitterEmailUnverified: caller.emailUnverified,
			TemplateID: p.TemplateID, TemplateVersionID: checked.versionID, FormSourceID: &source.ID, BodyVariables: send.variables,
			At: now, DeadlineAt: now.Add(mailApprovalDeadline), TrackingRequested: h.mailer.OperatorTrackingAlways(),
			Purpose: send.purpose.Purpose, Category: send.purpose.Category,
			TransactionalStatement: send.purpose.TransactionalStatement, CommercialContentAbsent: send.purpose.CommercialContentAbsent,
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

// queueToFormSource queues an approved request to a Forms source, locked:
// tracked as the request is (a.TrackingRequested, while SkyMail tracks every
// person's send), with what Keycloak and core said about its respondents
// before the lock (preferences).
func queueToFormSource(ctx context.Context, q *database.Queries, sends mailer.Queue, a database.MailApproval, preferences *mailer.TrackingPreferences) (uuid.UUID, error) {
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
	return sends.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: a.SubmitterSub, TemplateID: a.TemplateID, FormSourceID: a.FormSourceID, BodyVariables: a.BodyVariables, Recipients: to,
		Tracking: a.TrackingRequested, Preferences: preferences, Purpose: storedPurpose(a),
	})
}
