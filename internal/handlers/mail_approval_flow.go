package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/requiredvars"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// approvalCaller is who makes a request, as their token names them.
type approvalCaller struct {
	sub      string
	name     *string
	email    *string
	approver bool
}

func approvalCallerOf(c fiber.Ctx) (approvalCaller, error) {
	sub, _ := c.Locals("user_id").(string)
	if sub == "" {
		return approvalCaller{}, apperrors.ErrForbidden
	}
	caller := approvalCaller{sub: sub}
	if name, _ := c.Locals("user_name").(string); name != "" {
		caller.name = &name
	}
	if email, _ := c.Locals("user_email").(string); email != "" {
		caller.email = &email
	}
	roles, _ := c.Locals("roles").([]string)
	for _, role := range roles {
		if role == MailApproverRole {
			caller.approver = true
		}
	}
	return caller, nil
}

// recipient is the caller as a mail to them is addressed.
func (c approvalCaller) recipient() mailer.RecipientInfo {
	return mailer.RecipientInfo{FullName: deref(c.name), Email: deref(c.email)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func blankField(field string) error {
	return apperrors.ErrValidation.WithParams(map[string]interface{}{
		"errors": []validator.FieldError{{Field: field, Code: "required"}},
	})
}

// undecided is a state an approver or the submitter still has to act on, and
// the only ones a deadline expires.
func undecided(state database.MailApprovalState) bool {
	return state == database.MailApprovalStatePending || state == database.MailApprovalStateReturned
}

var mailApprovalStates = []database.MailApprovalState{
	database.MailApprovalStatePending, database.MailApprovalStateReturned, database.MailApprovalStateApproved,
	database.MailApprovalStateRejected, database.MailApprovalStateDeclined, database.MailApprovalStateExpired,
}

func parseApprovalState(raw string) (database.NullMailApprovalState, error) {
	state := database.MailApprovalState(strings.ToLower(strings.TrimSpace(raw)))
	if state == "" {
		return database.NullMailApprovalState{}, nil
	}
	if !stateIn(state, mailApprovalStates) {
		return database.NullMailApprovalState{}, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"state": "must be one of pending, returned, approved, rejected, declined, expired",
		})
	}
	return database.NullMailApprovalState{MailApprovalState: state, Valid: true}, nil
}

// approvalSend is what a request would send, as the submitter filled it in.
type approvalSend struct {
	templateID        uuid.UUID
	mailListID        *uuid.UUID
	recipientEmail    *string
	recipientFullName *string
	// A JSON object.
	variables []byte
}

func approvalSendOf(params requests.SubmitMailApproval) (approvalSend, error) {
	hasList := params.MailListID != nil && *params.MailListID != uuid.Nil
	email := strings.TrimSpace(params.RecipientEmail)
	if hasList == (email != "") {
		return approvalSend{}, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: "mail_list_id", Code: "exactly_one_of", Params: map[string]interface{}{
				"fields": []string{"mail_list_id", "recipient_email"},
			}}},
		})
	}
	variables := params.BodyVariables
	if variables == nil {
		variables = map[string]interface{}{}
	}
	raw, err := json.Marshal(variables)
	if err != nil {
		return approvalSend{}, err
	}
	send := approvalSend{templateID: params.TemplateID, variables: raw}
	if hasList {
		send.mailListID = params.MailListID
	} else {
		name := strings.TrimSpace(params.RecipientFullName)
		send.recipientEmail = &email
		send.recipientFullName = &name
	}
	return send, nil
}

// renderedFor is who a preview of a send is rendered for: its one recipient,
// or — a list's members each get their own — the submitter.
func (s approvalSend) renderedFor(submitter mailer.RecipientInfo) mailer.RecipientInfo {
	if s.recipientEmail != nil {
		return mailer.RecipientInfo{FullName: deref(s.recipientFullName), Email: *s.recipientEmail}
	}
	return submitter
}

func sendOfApproval(a database.MailApproval) approvalSend {
	return approvalSend{
		templateID:        a.TemplateID,
		mailListID:        a.MailListID,
		recipientEmail:    a.RecipientEmail,
		recipientFullName: a.RecipientFullName,
		variables:         a.BodyVariables,
	}
}

func submitterOf(a database.MailApproval) mailer.RecipientInfo {
	return mailer.RecipientInfo{FullName: deref(a.SubmitterName), Email: deref(a.SubmitterEmail)}
}

// checkedSend is a send a submission's checks passed, and the version of its
// template it is pinned to.
type checkedSend struct {
	template  database.Template
	versionID uuid.UUID
}

// checkSend checks a send the way a submission is checked: the template is in
// use and has a published version, the audience exists, and the values fill
// every Required variable and render.
func (h *mailApprovalHandlerImpl) checkSend(ctx context.Context, send approvalSend, submitter mailer.RecipientInfo) (checkedSend, error) {
	template, err := h.db.GetTemplateById(ctx, send.templateID)
	if isNotFound(err) || (err == nil && template.PublishedVersionID == nil) {
		return checkedSend{}, errApprovalTemplateUnavailable
	}
	if err != nil {
		return checkedSend{}, err
	}
	version, err := h.db.GetTemplateVersion(ctx, database.GetTemplateVersionParams{TemplateID: template.ID, ID: *template.PublishedVersionID})
	if err != nil {
		return checkedSend{}, err
	}

	if send.mailListID != nil {
		list, err := h.db.GetMailingListByIdIncludingArchived(ctx, *send.mailListID)
		switch {
		case err == nil && list.ArchivedAt != nil:
			return checkedSend{}, errApprovalAudienceUnavailable
		case isNotFound(err):
			group, err := h.kc.GetGroup(ctx, send.mailListID.String())
			if err != nil {
				return checkedSend{}, err
			}
			if group == nil {
				return checkedSend{}, errApprovalAudienceUnavailable
			}
		case err != nil:
			return checkedSend{}, err
		}
	}

	if err := checkApprovalValues(template, version, send.variables, send.renderedFor(submitter)); err != nil {
		return checkedSend{}, err
	}
	return checkedSend{template: template, versionID: version.TemplateVersionSummary.ID}, nil
}

// checkApprovalValues holds values to what a send of the template needs: a
// value for every Required variable — FullName and Email are the mailer's to
// give — and a render of the version with them.
func checkApprovalValues(template database.Template, version database.GetTemplateVersionRow, variables []byte, to mailer.RecipientInfo) error {
	var values map[string]interface{}
	if err := json.Unmarshal(variables, &values); err != nil {
		return err
	}
	filled := func(name string) bool {
		if name == "FullName" || name == "Email" {
			return true
		}
		value, ok := values[name]
		if !ok || value == nil {
			return false
		}
		if text, isText := value.(string); isText {
			return strings.TrimSpace(text) != ""
		}
		return true
	}
	var missing []requiredvars.Missing
	for _, variable := range template.ContractRequiredVariables {
		if !filled(variable.Name) {
			missing = append(missing, requiredvars.Missing{Name: variable.Name, Source: requiredvars.SourceContract, Reason: variable.Reason})
		}
	}
	for _, name := range template.OperatorRequiredVariables {
		if !filled(name) {
			missing = append(missing, requiredvars.Missing{Name: name, Source: requiredvars.SourceOperator})
		}
	}
	if len(missing) > 0 {
		sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
		return errApprovalVariablesMissing.WithParams(map[string]interface{}{"missing": missing})
	}

	if _, err := mailer.Render(version.TemplateVersionSummary.Subject, version.PlainTextContent, version.HtmlContent, variables, to); err != nil {
		return errApprovalUnrenderable.WithParams(map[string]interface{}{"error": err.Error()})
	}
	return nil
}

// Who may take an action on a request.
type approvalActor int

const (
	// An approver, on a request someone else submitted.
	byApprover approvalActor = iota
	// The request's submitter.
	bySubmitter
)

// approvalPrepared is what an action finds out before it takes the request's
// lock: a Keycloak group's members to send to, a resubmission's checks. Asking
// Keycloak while holding the lock would hold everyone else off for as long.
type approvalPrepared struct {
	// Set when the action sends to a Keycloak group: the group asked about.
	groupID *uuid.UUID
	// The group's members with an address; nil when the group is gone.
	groupMembers []mailer.RecipientInfo
	checked      checkedSend
}

// approvalAction is one action on a request.
type approvalAction struct {
	by approvalActor
	// The states it applies to.
	from []database.MailApprovalState
	// It sends the request: a Keycloak group's members are asked for first.
	sends bool
	// repeat says whether the request, locked, is already what the action
	// would make it, so doing it again changes nothing and answers 200.
	repeat  func(a database.MailApproval) bool
	prepare func(ctx context.Context, view database.GetMailApprovalRow, caller approvalCaller) (approvalPrepared, error)
	// do takes the action on the request, locked and in a state from allows,
	// and says whom to tell.
	do func(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, prepared approvalPrepared) (approvalNotice, error)
}

// act takes an action on the request the path names. The request is locked
// for the whole of it, so every action on one request happens in turn and a
// send is queued once; someone acting on it at that very moment is refused
// rather than kept waiting (mail_approval.busy). A request past its deadline
// is expired instead, whatever the action, and the answer says so.
func (h *mailApprovalHandlerImpl) act(c fiber.Ctx, action approvalAction) error {
	caller, err := approvalCallerOf(c)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	ctx := c.Context()

	view, err := h.db.GetMailApproval(ctx, id)
	if err != nil {
		return err
	}
	if err := authorizeApproval(caller, view.SubmitterSub, action.by); err != nil {
		return err
	}
	var prepared approvalPrepared
	if action.sends && view.MailListID != nil && !view.InternalMailList {
		prepared, err = h.groupRecipients(ctx, *view.MailListID)
		if err != nil {
			return err
		}
	}
	if action.prepare != nil {
		if prepared, err = action.prepare(ctx, view, caller); err != nil {
			return err
		}
	}

	var notice approvalNotice
	expired := false
	err = h.db.InTx(ctx, func(q *database.Queries) error {
		a, err := q.LockMailApproval(ctx, id)
		if err != nil {
			return lockError(err)
		}
		if undecided(a.State) && !h.now().Before(a.DeadlineAt) {
			expired = true
			notice, err = h.expireLocked(ctx, q, a)
			return err
		}
		if action.repeat != nil && action.repeat(a) {
			return nil
		}
		if !stateIn(a.State, action.from) {
			return errApprovalState.WithParams(map[string]interface{}{"state": a.State, "allowed": action.from})
		}
		notice, err = action.do(ctx, q, a, caller, prepared)
		return err
	})
	if err != nil {
		return err
	}

	if view, err = h.db.GetMailApproval(ctx, id); err != nil {
		return err
	}
	notification := h.deliver(ctx, view, notice)
	if expired {
		return errApprovalExpired.WithParams(map[string]interface{}{"deadline_at": view.DeadlineAt})
	}
	answer, err := h.approval(ctx, view)
	if err != nil {
		return err
	}
	answer.Notification = notification
	return c.JSON(answer)
}

// authorizeApproval lets an approver act on requests others submitted, and a
// submitter on their own. A request the caller may not even see is not found.
func authorizeApproval(caller approvalCaller, submitter string, by approvalActor) error {
	own := submitter == caller.sub
	switch {
	case by == byApprover && own:
		return errApprovalOwnRequest
	case by == bySubmitter && !own && caller.approver:
		return errApprovalNotSubmitter
	case by == bySubmitter && !own:
		return apperrors.ErrStatusNotFound
	}
	return nil
}

func stateIn(state database.MailApprovalState, states []database.MailApprovalState) bool {
	for _, s := range states {
		if s == state {
			return true
		}
	}
	return false
}

// lockError is what a request's lock failing means to the caller.
func lockError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return errApprovalBusy
	}
	return err
}

// groupRecipients asks Keycloak for a group's members with an address, as a
// send to a Keycloak group does.
func (h *mailApprovalHandlerImpl) groupRecipients(ctx context.Context, groupID uuid.UUID) (approvalPrepared, error) {
	prepared := approvalPrepared{groupID: &groupID}
	group, err := h.kc.GetGroup(ctx, groupID.String())
	if err != nil {
		return prepared, err
	}
	if group == nil {
		return prepared, nil
	}
	members, err := h.kc.GetGroupMembers(ctx, groupID.String())
	if err != nil {
		return prepared, err
	}
	prepared.groupMembers = []mailer.RecipientInfo{}
	for _, m := range members {
		if gocloak.PString(m.Email) == "" {
			continue
		}
		prepared.groupMembers = append(prepared.groupMembers, mailer.RecipientInfo{FullName: userFullName(m), Email: gocloak.PString(m.Email)})
	}
	return prepared, nil
}

// edit applies an approver's edit of the variables to a request, locked: it
// is checked as a submission's values are, written, and recorded with each
// variable it changed. No variables, or the request's own, is no edit.
func (h *mailApprovalHandlerImpl) edit(ctx context.Context, q *database.Queries, a database.MailApproval, variables map[string]interface{}, caller approvalCaller) (database.MailApproval, []MailApprovalChange, error) {
	if variables == nil {
		return a, nil, nil
	}
	raw, err := json.Marshal(variables)
	if err != nil {
		return a, nil, err
	}
	changes, err := variableChanges(a.BodyVariables, raw)
	if err != nil || len(changes) == 0 {
		return a, nil, err
	}

	template, err := q.GetTemplateByIdIncludingArchived(ctx, a.TemplateID)
	if err != nil {
		return a, nil, err
	}
	version, err := q.GetTemplateVersion(ctx, database.GetTemplateVersionParams{TemplateID: a.TemplateID, ID: a.TemplateVersionID})
	if err != nil {
		return a, nil, err
	}
	edited := sendOfApproval(a)
	edited.variables = raw
	if err := checkApprovalValues(template, version, raw, edited.renderedFor(submitterOf(a))); err != nil {
		return a, nil, err
	}

	a, err = q.SetMailApprovalVariables(ctx, database.SetMailApprovalVariablesParams{BodyVariables: raw, At: h.now(), ID: a.ID})
	if err != nil {
		return a, nil, err
	}
	recorded, err := json.Marshal(changes)
	if err != nil {
		return a, nil, err
	}
	_, err = q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       database.MailApprovalEventKindEdited,
		ActorSub:   &caller.sub,
		ActorName:  caller.name,
		Changes:    recorded,
		At:         h.now(),
	})
	return a, changes, err
}

// send queues a request, locked, through the send path, sent by its
// submitter: exactly its values, of the template version it is pinned to, to
// its audience as it is now. The template and an internal list are
// share-locked until the request's transaction ends, so neither can be
// published over or archived between these checks and the queueing; the
// mailer reads the template row, which is a copy of the pinned version.
func (h *mailApprovalHandlerImpl) send(ctx context.Context, q *database.Queries, a database.MailApproval, prepared approvalPrepared) (uuid.UUID, error) {
	template, err := q.ShareLockTemplate(ctx, a.TemplateID)
	if err != nil {
		return uuid.Nil, err
	}
	if template.ArchivedAt != nil {
		return uuid.Nil, atSend(errApprovalTemplateUnavailable)
	}
	if template.PublishedVersionID == nil || *template.PublishedVersionID != a.TemplateVersionID {
		return uuid.Nil, errApprovalTemplateRepublished.WithParams(map[string]interface{}{
			"submitted_version_id": a.TemplateVersionID,
			"published_version_id": template.PublishedVersionID,
		})
	}

	if a.RecipientEmail != nil {
		return h.mailer.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy:            a.SubmitterSub,
			TemplateID:        &a.TemplateID,
			BodyVariables:     a.BodyVariables,
			RecipientFullName: deref(a.RecipientFullName),
			RecipientEmail:    *a.RecipientEmail,
		})
	}

	list, err := q.ShareLockMailingList(ctx, *a.MailListID)
	switch {
	case err == nil:
		if list.ArchivedAt != nil {
			return uuid.Nil, atSend(errApprovalAudienceUnavailable)
		}
		recipients, err := q.CountRecipientsByMailingListId(ctx, list.ID)
		if err != nil {
			return uuid.Nil, err
		}
		if recipients == 0 {
			return uuid.Nil, errApprovalAudienceEmpty
		}
		taskID, err := h.mailer.Enqueue(ctx, database.CreateMailTaskParams{
			SentBy:        a.SubmitterSub,
			TemplateID:    &a.TemplateID,
			MailListID:    a.MailListID,
			BodyVariables: a.BodyVariables,
		})
		if err == nil && taskID == uuid.Nil {
			return uuid.Nil, errApprovalAudienceEmpty
		}
		return taskID, err
	case !isNotFound(err):
		return uuid.Nil, err
	}

	// A Keycloak group, whose members were asked for before the lock.
	if prepared.groupID == nil || *prepared.groupID != *a.MailListID {
		return uuid.Nil, errApprovalBusy
	}
	if prepared.groupMembers == nil {
		return uuid.Nil, atSend(errApprovalAudienceUnavailable)
	}
	if len(prepared.groupMembers) == 0 {
		return uuid.Nil, errApprovalAudienceEmpty
	}
	return h.mailer.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy:        a.SubmitterSub,
		TemplateID:    a.TemplateID,
		MailListID:    a.MailListID,
		BodyVariables: a.BodyVariables,
		Recipients:    prepared.groupMembers,
	})
}

// decide moves a request, locked, to state and records the event that did.
func (h *mailApprovalHandlerImpl) decide(ctx context.Context, q *database.Queries, a database.MailApproval, state database.MailApprovalState, kind database.MailApprovalEventKind, caller approvalCaller, note string, taskID *uuid.UUID) error {
	if _, err := q.SetMailApprovalState(ctx, database.SetMailApprovalStateParams{State: state, TaskID: taskID, At: h.now(), ID: a.ID}); err != nil {
		return err
	}
	_, err := q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       kind,
		ActorSub:   &caller.sub,
		ActorName:  caller.name,
		Note:       optionalText(note),
		TaskID:     taskID,
		At:         h.now(),
	})
	return err
}

func optionalText(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// resubmit makes a rejected or declined request, locked, pending again as the
// submitter filled it in now, and records what changed.
func (h *mailApprovalHandlerImpl) resubmit(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, send approvalSend, checked checkedSend) (approvalNotice, error) {
	changes, err := variableChanges(a.BodyVariables, send.variables)
	if err != nil {
		return approvalNotice{}, err
	}
	if a.TemplateID != checked.template.ID || a.TemplateVersionID != checked.versionID {
		before, _ := json.Marshal(map[string]uuid.UUID{"id": a.TemplateID, "version_id": a.TemplateVersionID})
		after, _ := json.Marshal(map[string]uuid.UUID{"id": checked.template.ID, "version_id": checked.versionID})
		changes = append([]MailApprovalChange{{Field: "template", Before: before, After: after}}, changes...)
	}
	if before, after := audienceJSON(sendOfApproval(a)), audienceJSON(send); !bytesEqualJSON(before, after) {
		changes = append([]MailApprovalChange{{Field: "audience", Before: before, After: after}}, changes...)
	}

	now := h.now()
	if _, err := q.ResubmitMailApproval(ctx, database.ResubmitMailApprovalParams{
		TemplateID:        checked.template.ID,
		TemplateVersionID: checked.versionID,
		MailListID:        send.mailListID,
		RecipientEmail:    send.recipientEmail,
		RecipientFullName: send.recipientFullName,
		BodyVariables:     send.variables,
		At:                now,
		DeadlineAt:        now.Add(mailApprovalDeadline),
		ID:                a.ID,
	}); err != nil {
		return approvalNotice{}, err
	}
	recorded, err := json.Marshal(changes)
	if err != nil {
		return approvalNotice{}, err
	}
	_, err = q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       database.MailApprovalEventKindResubmitted,
		ActorSub:   &caller.sub,
		ActorName:  caller.name,
		Changes:    recorded,
		At:         now,
	})
	return approvalNotice{kind: noticeRequested, by: caller.sub}, err
}

func audienceJSON(s approvalSend) json.RawMessage {
	var raw []byte
	if s.mailListID != nil {
		raw, _ = json.Marshal(map[string]interface{}{"mail_list_id": s.mailListID})
	} else {
		raw, _ = json.Marshal(map[string]interface{}{"recipient_email": s.recipientEmail, "recipient_full_name": s.recipientFullName})
	}
	return raw
}

// variableChanges is each variable whose value differs between two JSON
// objects, by name, with its value before and after — null where it had none.
// Values are compared as JSON values, not as text.
func variableChanges(before, after []byte) ([]MailApprovalChange, error) {
	var was, is map[string]interface{}
	if err := json.Unmarshal(before, &was); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(after, &is); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(was)+len(is))
	for name := range was {
		names = append(names, name)
	}
	for name := range is {
		if _, seen := was[name]; !seen {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	changes := []MailApprovalChange{}
	for _, name := range names {
		old, had := was[name]
		now, has := is[name]
		if had == has && reflect.DeepEqual(old, now) {
			continue
		}
		change := MailApprovalChange{Field: "variable", Name: &name}
		if had {
			change.Before, _ = json.Marshal(old)
		}
		if has {
			change.After, _ = json.Marshal(now)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func bytesEqualJSON(a, b []byte) bool {
	var x, y interface{}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// expireLocked expires an undecided request, locked, past its deadline.
func (h *mailApprovalHandlerImpl) expireLocked(ctx context.Context, q *database.Queries, a database.MailApproval) (approvalNotice, error) {
	if _, err := q.SetMailApprovalState(ctx, database.SetMailApprovalStateParams{State: database.MailApprovalStateExpired, At: h.now(), ID: a.ID}); err != nil {
		return approvalNotice{}, err
	}
	_, err := q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       database.MailApprovalEventKindExpired,
		At:         h.now(),
	})
	return approvalNotice{kind: noticeResolved, decision: "expired", decidedBy: "SkyMail", note: expiredNote}, err
}

// expireIfDue expires one request if it is undecided past its deadline and
// no one is acting on it right now; someone who is will find it expired.
func (h *mailApprovalHandlerImpl) expireIfDue(ctx context.Context, id uuid.UUID) error {
	var notice approvalNotice
	err := h.db.InTx(ctx, func(q *database.Queries) error {
		a, err := q.LockMailApproval(ctx, id)
		if err != nil {
			return err
		}
		if !undecided(a.State) || h.now().Before(a.DeadlineAt) {
			return nil
		}
		notice, err = h.expireLocked(ctx, q, a)
		return err
	})
	if errors.Is(lockError(err), errApprovalBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	if notice.kind != noticeNone {
		view, err := h.db.GetMailApproval(ctx, id)
		if err != nil {
			return err
		}
		h.deliver(ctx, view, notice)
	}
	return nil
}

// A sweep expires at most this many requests; the next one goes on.
const expirySweepBatch = 100

func (h *mailApprovalHandlerImpl) ExpireDue(ctx context.Context) (int, error) {
	expired := 0
	for expired < expirySweepBatch {
		var id uuid.UUID
		var notice approvalNotice
		err := h.db.InTx(ctx, func(q *database.Queries) error {
			a, err := q.LockDueMailApproval(ctx, h.now())
			if err != nil {
				return err
			}
			id = a.ID
			notice, err = h.expireLocked(ctx, q, a)
			return err
		})
		if isNotFound(err) {
			return expired, nil
		}
		if err != nil {
			return expired, err
		}
		expired++
		view, err := h.db.GetMailApproval(ctx, id)
		if err != nil {
			return expired, err
		}
		h.deliver(ctx, view, notice)
	}
	return expired, nil
}

// approval is a request whole, as reading it answers.
func (h *mailApprovalHandlerImpl) approval(ctx context.Context, view database.GetMailApprovalRow) (MailApproval, error) {
	events, err := h.db.ListMailApprovalEvents(ctx, view.ID)
	if err != nil {
		return MailApproval{}, err
	}
	history := make([]MailApprovalEvent, len(events))
	for i, event := range events {
		history[i] = approvalEventOf(event)
	}
	var last *MailApprovalEvent
	if len(history) > 0 {
		last = &history[len(history)-1]
	}

	answer := MailApproval{
		MailApprovalItem: approvalItem(view, h.approvalGroupNames(ctx, []database.GetMailApprovalRow{view}), last),
		RecipientCount:   h.recipientCount(ctx, view),
		History:          history,
	}
	version, err := h.db.GetTemplateVersion(ctx, database.GetTemplateVersionParams{TemplateID: view.TemplateID, ID: view.TemplateVersionID})
	if err != nil {
		return MailApproval{}, err
	}
	to := viewSend(view).renderedFor(mailer.RecipientInfo{FullName: deref(view.SubmitterName), Email: deref(view.SubmitterEmail)})
	rendered, err := mailer.Render(version.TemplateVersionSummary.Subject, version.PlainTextContent, version.HtmlContent, view.BodyVariables, to)
	if err != nil {
		message := err.Error()
		answer.PreviewError = &message
	} else {
		answer.Preview = &MailApprovalPreview{
			Subject:     rendered.Subject,
			HTML:        rendered.HTML,
			PlainText:   rendered.PlainText,
			RenderedFor: MailApprovalRecipient{FullName: to.FullName, Email: to.Email},
		}
	}
	return answer, nil
}

func viewSend(view database.GetMailApprovalRow) approvalSend {
	return approvalSend{
		templateID:        view.TemplateID,
		mailListID:        view.MailListID,
		recipientEmail:    view.RecipientEmail,
		recipientFullName: view.RecipientFullName,
		variables:         view.BodyVariables,
	}
}

func approvalItem(view database.GetMailApprovalRow, groupNames map[uuid.UUID]*string, last *MailApprovalEvent) MailApprovalItem {
	return MailApprovalItem{
		ID:    view.ID,
		State: string(view.State),
		Submitter: MailApprovalSubmitter{
			Sub:   view.SubmitterSub,
			Name:  view.SubmitterName,
			Email: view.SubmitterEmail,
		},
		Template: MailApprovalTemplate{
			ID:          view.TemplateID,
			VersionID:   view.TemplateVersionID,
			Name:        view.TemplateName,
			Key:         view.TemplateKey,
			Republished: view.TemplatePublishedVersionID == nil || *view.TemplatePublishedVersionID != view.TemplateVersionID,
		},
		Audience:      approvalAudience(view, groupNames),
		BodyVariables: view.BodyVariables,
		CreatedAt:     view.CreatedAt,
		SubmittedAt:   view.SubmittedAt,
		DeadlineAt:    view.DeadlineAt,
		UpdatedAt:     view.UpdatedAt,
		TaskID:        view.TaskID,
		LastEvent:     last,
	}
}

func approvalAudience(view database.GetMailApprovalRow, groupNames map[uuid.UUID]*string) SendAudience {
	switch {
	case view.MailListID == nil:
		return SendAudience{Kind: audienceSingle, RecipientFullName: view.RecipientFullName, RecipientEmail: view.RecipientEmail}
	case view.InternalMailList:
		source := sourceInternal
		return SendAudience{Kind: audienceMailingList, MailListID: view.MailListID, Name: view.MailListName, Source: &source}
	default:
		source := sourceKeycloak
		return SendAudience{Kind: audienceMailingList, MailListID: view.MailListID, Name: groupNames[*view.MailListID], Source: &source}
	}
}

func approvalEventOf(e database.MailApprovalEvent) MailApprovalEvent {
	event := MailApprovalEvent{
		Seq:     e.Seq,
		Kind:    string(e.Kind),
		Note:    e.Note,
		Changes: []MailApprovalChange{},
		TaskID:  e.TaskID,
		At:      e.CreatedAt,
	}
	if e.ActorSub != nil {
		event.Actor = &MailApprovalPerson{Sub: *e.ActorSub, Name: e.ActorName}
	}
	if e.Changes != nil {
		if err := json.Unmarshal(e.Changes, &event.Changes); err != nil {
			log.Warn().Err(err).Str("approval_id", e.ApprovalID.String()).Int("seq", e.Seq).Msg("unreadable changes on a mail approval event")
		}
	}
	return event
}

// approvalGroupNames names the Keycloak groups among the requests, asking once
// per group within one budget. A name is a nicety: one Keycloak cannot give in
// time is left null.
func (h *mailApprovalHandlerImpl) approvalGroupNames(ctx context.Context, views []database.GetMailApprovalRow) map[uuid.UUID]*string {
	ctx, cancel := context.WithTimeout(ctx, keycloakGroupNameBudget)
	defer cancel()

	names := map[uuid.UUID]*string{}
	for _, view := range views {
		if ctx.Err() != nil {
			break
		}
		if view.MailListID == nil || view.InternalMailList {
			continue
		}
		id := *view.MailListID
		if _, done := names[id]; done {
			continue
		}
		names[id] = nil
		group, err := h.kc.GetGroup(ctx, id.String())
		if err != nil {
			log.Warn().Err(err).Str("group_id", id.String()).Msg("could not name the Keycloak group of an approval request")
			continue
		}
		if group != nil && group.Name != nil {
			names[id] = group.Name
		}
	}
	return names
}

// recipientCount is how many a request would reach now; nil when Keycloak
// cannot say within its budget.
func (h *mailApprovalHandlerImpl) recipientCount(ctx context.Context, view database.GetMailApprovalRow) *int64 {
	var count int64
	switch {
	case view.MailListID == nil:
		count = 1
	case view.InternalMailList:
		n, err := h.db.CountRecipientsByMailingListId(ctx, *view.MailListID)
		if err != nil {
			log.Warn().Err(err).Str("approval_id", view.ID.String()).Msg("could not count an approval request's list")
			return nil
		}
		count = n
	default:
		ctx, cancel := context.WithTimeout(ctx, approvalKeycloakBudget)
		defer cancel()
		members, err := h.kc.GetGroupMembers(ctx, view.MailListID.String())
		if err != nil {
			log.Warn().Err(err).Str("approval_id", view.ID.String()).Msg("could not count an approval request's Keycloak group")
			return nil
		}
		for _, m := range members {
			if gocloak.PString(m.Email) != "" {
				count++
			}
		}
	}
	return &count
}
