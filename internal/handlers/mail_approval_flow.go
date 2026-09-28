package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

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
	sub   string
	name  *string
	email *string
	// The token carried an address Keycloak had not verified; email is nil.
	emailUnverified bool
	approver        bool
	roles           []string
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
	caller.emailUnverified, _ = c.Locals("user_email_unverified").(bool)
	caller.roles, _ = c.Locals("roles").([]string)
	caller.approver = caller.has(MailApproverRole)
	return caller, nil
}

func (c approvalCaller) has(role string) bool {
	for _, r := range c.roles {
		if r == role {
			return true
		}
	}
	return false
}

// mayRead lets the caller submit send only if they can read what it submits
// (Yusuf, 2026-09-24): its template, and its list when it goes to one — the
// rule the screens show, held here too. The roles missing are named.
func (c approvalCaller) mayRead(send approvalSend) error {
	var missing []string
	if !c.has(templatesReadRole) {
		missing = append(missing, templatesReadRole)
	}
	if send.mailListID != nil && !c.has(listsReadRole) {
		missing = append(missing, listsReadRole)
	}
	if len(missing) > 0 {
		return apperrors.ErrForbidden.WithParams(map[string]interface{}{"missing_roles": missing})
	}
	return nil
}

// listNamedIn is as much of a send as a body that has not been checked yet
// shows: whether it names a list. A body that does not parse names none.
func listNamedIn(body []byte) approvalSend {
	var peek struct {
		MailListID *uuid.UUID `json:"mail_list_id"`
	}
	if json.Unmarshal(body, &peek) != nil || peek.MailListID == nil || *peek.MailListID == uuid.Nil {
		return approvalSend{}
	}
	return approvalSend{mailListID: peek.MailListID}
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
	templateID uuid.UUID
	mailListID *uuid.UUID
	// The people it goes to, in order; none when it goes to a list.
	recipients []mailer.RecipientInfo
	// A JSON object.
	variables []byte
}

func approvalSendOf(params requests.SubmitMailApproval) (approvalSend, error) {
	recipients, err := recipientsOf(params.Recipients)
	if err != nil {
		return approvalSend{}, err
	}
	hasList := params.MailListID != nil && *params.MailListID != uuid.Nil
	if hasList == (len(recipients) > 0) {
		return approvalSend{}, errNotOneAudience()
	}
	variables := params.BodyVariables
	if variables == nil {
		variables = map[string]interface{}{}
	}
	raw, err := json.Marshal(variables)
	if err != nil {
		return approvalSend{}, err
	}
	send := approvalSend{templateID: params.TemplateID, recipients: recipients, variables: raw}
	if hasList {
		send.mailListID = params.MailListID
	}
	return send, nil
}

// errNotOneAudience refuses a submission that names no audience, or both a
// list and recipients.
func errNotOneAudience() error {
	return apperrors.ErrValidation.WithParams(map[string]interface{}{
		"errors": []validator.FieldError{{Field: "mail_list_id", Code: "exactly_one_of", Params: map[string]interface{}{
			"fields": []string{"mail_list_id", "recipients"},
		}}},
	})
}

// recipientsOf is the people a submission names, trimmed, in order. An
// address named twice, whatever its case, is refused: everyone is sent to
// once.
func recipientsOf(people []requests.MailApprovalRecipient) ([]mailer.RecipientInfo, error) {
	recipients := make([]mailer.RecipientInfo, len(people))
	first := map[string]int{}
	var duplicates []validator.FieldError
	for i, r := range people {
		recipients[i] = mailer.RecipientInfo{FullName: strings.TrimSpace(r.FullName), Email: strings.TrimSpace(r.Email)}
		address := strings.ToLower(recipients[i].Email)
		if j, seen := first[address]; seen {
			duplicates = append(duplicates, validator.FieldError{
				Field: recipientField(i), Code: "duplicate", Params: map[string]interface{}{"first": recipientField(j)},
			})
			continue
		}
		first[address] = i
	}
	if len(duplicates) > 0 {
		return nil, apperrors.ErrValidation.WithParams(map[string]interface{}{"errors": duplicates})
	}
	return recipients, nil
}

func recipientField(i int) string {
	return "recipients[" + strconv.Itoa(i) + "].email"
}

// duplicateRecipientsError is what an address named twice means to the
// caller when Postgres, not recipientsOf, found it: its lower() and Go's
// strings.ToLower can differ on an unusual address. Which rows clashed is
// Postgres's to know, so the error names the list.
func duplicateRecipientsError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "mail_approval_recipients_email_once" {
		return apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: "recipients", Code: "duplicate"}},
		})
	}
	return err
}

// renderedFor is who a preview of a send is rendered for: its first person,
// or — a list's members each get their own — the submitter.
func (s approvalSend) renderedFor(submitter mailer.RecipientInfo) mailer.RecipientInfo {
	if len(s.recipients) > 0 {
		return s.recipients[0]
	}
	return submitter
}

// sendOfView is what a request would send, as it was read.
func sendOfView(view database.GetMailApprovalRow) approvalSend {
	return approvalSend{
		templateID: view.MailApproval.TemplateID,
		mailListID: view.MailApproval.MailListID,
		recipients: recipientsOfView(view),
		variables:  view.MailApproval.BodyVariables,
	}
}

// recipientsOfView is the people a request goes to, as it was read.
func recipientsOfView(view database.GetMailApprovalRow) []mailer.RecipientInfo {
	recipients := make([]mailer.RecipientInfo, len(view.RecipientEmails))
	for i, email := range view.RecipientEmails {
		recipients[i] = mailer.RecipientInfo{FullName: view.RecipientFullNames[i], Email: email}
	}
	return recipients
}

// lockedSend is what a request, locked, would send: its row and its people as
// q's transaction sees them.
func lockedSend(ctx context.Context, q *database.Queries, a database.MailApproval) (approvalSend, error) {
	rows, err := q.ListMailApprovalRecipients(ctx, a.ID)
	if err != nil {
		return approvalSend{}, err
	}
	recipients := make([]mailer.RecipientInfo, len(rows))
	for i, row := range rows {
		recipients[i] = mailer.RecipientInfo{FullName: row.FullName, Email: row.Email}
	}
	return approvalSend{templateID: a.TemplateID, mailListID: a.MailListID, recipients: recipients, variables: a.BodyVariables}, nil
}

// setRecipients makes recipients the people a request goes to, in q's
// transaction and in order, in place of whoever it went to before.
func setRecipients(ctx context.Context, q *database.Queries, id uuid.UUID, recipients []mailer.RecipientInfo) error {
	if err := q.ClearMailApprovalRecipients(ctx, id); err != nil {
		return err
	}
	if len(recipients) == 0 {
		return nil
	}
	params := database.AddMailApprovalRecipientsParams{
		ApprovalID: id,
		Emails:     make([]string, len(recipients)),
		FullNames:  make([]string, len(recipients)),
	}
	for i, r := range recipients {
		params.Emails[i], params.FullNames[i] = r.Email, r.FullName
	}
	return duplicateRecipientsError(q.AddMailApprovalRecipients(ctx, params))
}

// submitterOf is a request's submitter as a mail to them is addressed.
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
	// An approver, on any request.
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
	// A resubmission's send, as its checks passed it.
	checked checkedSend
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
	repeat func(a database.MailApproval) bool
	// check checks what the action was given before the lock is taken.
	check func(ctx context.Context, view database.GetMailApprovalRow, caller approvalCaller) (checkedSend, error)
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
	if err := authorizeApproval(caller, view.MailApproval.SubmitterSub, action.by); err != nil {
		return err
	}
	var prepared approvalPrepared
	if action.sends && view.MailApproval.MailListID != nil && !view.InternalMailList {
		prepared, err = h.groupRecipients(ctx, *view.MailApproval.MailListID)
		if err != nil {
			return err
		}
	}
	if action.check != nil {
		if prepared.checked, err = action.check(ctx, view, caller); err != nil {
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
	if action.sends {
		h.mailer.Wake()
	}

	if view, err = h.db.GetMailApproval(ctx, id); err != nil {
		return err
	}
	notification := h.deliver(ctx, view, notice)
	if expired {
		return errApprovalExpired.WithParams(map[string]interface{}{"deadline_at": view.MailApproval.DeadlineAt})
	}
	answer, err := h.approval(ctx, view)
	if err != nil {
		return err
	}
	answer.Notification = notification
	return c.JSON(answer)
}

// authorizeApproval lets an approver decide any request — their own too
// (Yusuf, 2026-09-23): the history names who submitted and who decided, so a
// self-approval shows as one — and a submitter act on their own. The route
// has already required the approver's role. A request the caller may not even
// see is not found.
func authorizeApproval(caller approvalCaller, submitter string, by approvalActor) error {
	own := submitter == caller.sub
	switch {
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
	prepared.groupMembers = membersWithAddress(members)
	return prepared, nil
}

// membersWithAddress is a Keycloak group's members as a send to it addresses
// them: those with an e-mail address, each by name.
func membersWithAddress(members []*gocloak.User) []mailer.RecipientInfo {
	recipients := []mailer.RecipientInfo{}
	for _, m := range members {
		if email := gocloak.PString(m.Email); email != "" {
			recipients = append(recipients, mailer.RecipientInfo{FullName: userFullName(m), Email: email})
		}
	}
	return recipients
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
	edited, err := lockedSend(ctx, q, a)
	if err != nil {
		return a, nil, err
	}
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
// submitter — exactly its values, of the template version it is pinned to, to
// its audience as it is now — and records the sends on it, in order: a list's
// one send, or one per person, as POST /mail_tasks/single sends to one. They
// are queued in the request's transaction, so they exist only if the decision
// commits, all or none; act wakes the dispatcher after. The template and an
// internal list are share-locked until then, so neither can be published over
// or archived between these checks and the queueing; the mailer reads the
// template row, a copy of the pinned version.
func (h *mailApprovalHandlerImpl) send(ctx context.Context, q *database.Queries, a database.MailApproval, prepared approvalPrepared) ([]uuid.UUID, error) {
	taskIDs, err := h.queue(ctx, q, a, prepared)
	if err != nil {
		return nil, err
	}
	return taskIDs, q.AddMailApprovalTasks(ctx, database.AddMailApprovalTasksParams{ApprovalID: a.ID, TaskIds: taskIDs})
}

// queue queues a request's sends, locked, and returns them in order.
func (h *mailApprovalHandlerImpl) queue(ctx context.Context, q *database.Queries, a database.MailApproval, prepared approvalPrepared) ([]uuid.UUID, error) {
	template, err := q.ShareLockTemplate(ctx, a.TemplateID)
	if err != nil {
		return nil, err
	}
	if template.ArchivedAt != nil {
		return nil, atSend(errApprovalTemplateUnavailable)
	}
	if template.PublishedVersionID == nil || *template.PublishedVersionID != a.TemplateVersionID {
		return nil, errApprovalTemplateRepublished.WithParams(map[string]interface{}{
			"submitted_version_id": a.TemplateVersionID,
			"published_version_id": template.PublishedVersionID,
		})
	}

	sends := h.mailer.Queue(q)
	if a.MailListID == nil {
		return queueToPeople(ctx, q, sends, a)
	}
	taskID, err := queueToList(ctx, q, sends, a, prepared)
	if err != nil {
		return nil, err
	}
	return []uuid.UUID{taskID}, nil
}

// queueToPeople queues a request, locked, as one single send per person, in
// their order.
func queueToPeople(ctx context.Context, q *database.Queries, sends mailer.Queue, a database.MailApproval) ([]uuid.UUID, error) {
	send, err := lockedSend(ctx, q, a)
	if err != nil {
		return nil, err
	}
	if len(send.recipients) == 0 {
		return nil, errApprovalAudienceEmpty
	}
	taskIDs := make([]uuid.UUID, len(send.recipients))
	for i, to := range send.recipients {
		if taskIDs[i], err = sends.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy:            a.SubmitterSub,
			TemplateID:        &a.TemplateID,
			BodyVariables:     a.BodyVariables,
			RecipientFullName: to.FullName,
			RecipientEmail:    to.Email,
		}); err != nil {
			return nil, err
		}
	}
	return taskIDs, nil
}

// queueToList queues a request, locked, as the one send to its internal list
// or Keycloak group.
func queueToList(ctx context.Context, q *database.Queries, sends mailer.Queue, a database.MailApproval, prepared approvalPrepared) (uuid.UUID, error) {
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
		taskID, err := sends.Enqueue(ctx, database.CreateMailTaskParams{
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

	// A Keycloak group, whose members were asked for before the lock — unless
	// the request's audience changed in between.
	if prepared.groupID == nil || *prepared.groupID != *a.MailListID {
		return uuid.Nil, errApprovalChanged
	}
	if prepared.groupMembers == nil {
		return uuid.Nil, atSend(errApprovalAudienceUnavailable)
	}
	if len(prepared.groupMembers) == 0 {
		return uuid.Nil, errApprovalAudienceEmpty
	}
	return sends.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy:        a.SubmitterSub,
		TemplateID:    a.TemplateID,
		MailListID:    a.MailListID,
		BodyVariables: a.BodyVariables,
		Recipients:    prepared.groupMembers,
	})
}

// approvalStep is what a decision does to a request: the state it moves it
// to, the event that records it with its note and send — the first, when it
// queued one per person — and, for a return, the new deadline.
type approvalStep struct {
	state    database.MailApprovalState
	kind     database.MailApprovalEventKind
	note     string
	taskID   *uuid.UUID
	deadline *time.Time
}

// decide takes step on a request, locked, by caller.
func (h *mailApprovalHandlerImpl) decide(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, step approvalStep) error {
	if _, err := q.SetMailApprovalState(ctx, database.SetMailApprovalStateParams{
		State: step.state, DeadlineAt: step.deadline, At: h.now(), ID: a.ID,
	}); err != nil {
		return err
	}
	_, err := q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       step.kind,
		ActorSub:   &caller.sub,
		ActorName:  caller.name,
		Note:       optionalText(step.note),
		TaskID:     step.taskID,
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
	was, err := lockedSend(ctx, q, a)
	if err != nil {
		return approvalNotice{}, err
	}
	if before, after := audienceJSON(was), audienceJSON(send); !bytesEqualJSON(before, after) {
		changes = append([]MailApprovalChange{{Field: "audience", Before: before, After: after}}, changes...)
	}

	now := h.now()
	if _, err := q.ResubmitMailApproval(ctx, database.ResubmitMailApprovalParams{
		TemplateID:        checked.template.ID,
		TemplateVersionID: checked.versionID,
		MailListID:        send.mailListID,
		BodyVariables:     send.variables,
		At:                now,
		DeadlineAt:        now.Add(mailApprovalDeadline),
		ID:                a.ID,
	}); err != nil {
		return approvalNotice{}, err
	}
	if err := setRecipients(ctx, q, a.ID, send.recipients); err != nil {
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

// audienceJSON is who a send goes to, as a change records it: {mail_list_id}
// or {recipients: [{email, full_name}]}.
func audienceJSON(s approvalSend) json.RawMessage {
	var raw []byte
	if s.mailListID != nil {
		raw, _ = json.Marshal(map[string]interface{}{"mail_list_id": s.mailListID})
	} else {
		raw, _ = json.Marshal(map[string]interface{}{"recipients": approvalRecipients(s.recipients)})
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
	return approvalNotice{kind: noticeResolved, decision: decisionExpired, decidedBy: "SkyMail"}, err
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
	events, err := h.db.ListMailApprovalEvents(ctx, view.MailApproval.ID)
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
		MailApprovalItem: approvalItem(view, h.approvalGroupNames(ctx, []database.GetMailApprovalRow{view}), last, h.now()),
		RecipientCount:   h.recipientCount(ctx, view),
		History:          history,
	}
	version, err := h.db.GetTemplateVersion(ctx, database.GetTemplateVersionParams{TemplateID: view.MailApproval.TemplateID, ID: view.MailApproval.TemplateVersionID})
	if err != nil {
		return MailApproval{}, err
	}
	to := sendOfView(view).renderedFor(submitterOf(view.MailApproval))
	answer.PreviewRecipient = MailApprovalRecipient{FullName: to.FullName, Email: to.Email}
	rendered, err := mailer.Render(version.TemplateVersionSummary.Subject, version.PlainTextContent, version.HtmlContent, view.MailApproval.BodyVariables, to)
	if err != nil {
		message := err.Error()
		answer.PreviewError = &message
	} else {
		answer.Preview = &MailApprovalPreview{
			Subject:     rendered.Subject,
			HTML:        rendered.HTML,
			PlainText:   rendered.PlainText,
			RenderedFor: answer.PreviewRecipient,
		}
	}
	return answer, nil
}

// effectiveState is the state a request is in as of now: one undecided past
// its deadline is expired, whether or not the sweep has written it yet.
func effectiveState(state database.MailApprovalState, deadline, now time.Time) database.MailApprovalState {
	if undecided(state) && !now.Before(deadline) {
		return database.MailApprovalStateExpired
	}
	return state
}

func approvalItem(view database.GetMailApprovalRow, groupNames map[uuid.UUID]*string, last *MailApprovalEvent, now time.Time) MailApprovalItem {
	return MailApprovalItem{
		ID:    view.MailApproval.ID,
		State: string(effectiveState(view.MailApproval.State, view.MailApproval.DeadlineAt, now)),
		Submitter: MailApprovalSubmitter{
			Sub:   view.MailApproval.SubmitterSub,
			Name:  view.MailApproval.SubmitterName,
			Email: view.MailApproval.SubmitterEmail,
		},
		Template: MailApprovalTemplate{
			ID:          view.MailApproval.TemplateID,
			VersionID:   view.MailApproval.TemplateVersionID,
			Name:        view.TemplateName,
			Key:         view.TemplateKey,
			Republished: view.TemplatePublishedVersionID == nil || *view.TemplatePublishedVersionID != view.MailApproval.TemplateVersionID,
		},
		Audience:      approvalAudience(view, groupNames),
		Recipients:    approvalRecipients(recipientsOfView(view)),
		BodyVariables: view.MailApproval.BodyVariables,
		CreatedAt:     view.MailApproval.CreatedAt,
		SubmittedAt:   view.MailApproval.SubmittedAt,
		DeadlineAt:    view.MailApproval.DeadlineAt,
		UpdatedAt:     view.MailApproval.UpdatedAt,
		TaskIDs:       view.TaskIds,
		LastEvent:     last,
	}
}

func approvalRecipients(recipients []mailer.RecipientInfo) []MailApprovalRecipient {
	out := make([]MailApprovalRecipient, len(recipients))
	for i, r := range recipients {
		out[i] = MailApprovalRecipient{FullName: r.FullName, Email: r.Email}
	}
	return out
}

// approvalAudience is who a request goes to, as a send's audience reads: a
// list; one person, as a single send names them; or several people, whom the
// request's recipients name.
func approvalAudience(view database.GetMailApprovalRow, groupNames map[uuid.UUID]*string) SendAudience {
	switch {
	case view.MailApproval.MailListID == nil && len(view.RecipientEmails) == 1:
		return SendAudience{Kind: audienceSingle, RecipientFullName: &view.RecipientFullNames[0], RecipientEmail: &view.RecipientEmails[0]}
	case view.MailApproval.MailListID == nil:
		return SendAudience{Kind: audiencePeople}
	case view.InternalMailList:
		source := sourceInternal
		return SendAudience{Kind: audienceMailingList, MailListID: view.MailApproval.MailListID, Name: view.MailListName, Source: &source}
	default:
		source := sourceKeycloak
		return SendAudience{Kind: audienceMailingList, MailListID: view.MailApproval.MailListID, Name: groupNames[*view.MailApproval.MailListID], Source: &source}
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
		if view.MailApproval.MailListID == nil || view.InternalMailList {
			continue
		}
		id := *view.MailApproval.MailListID
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
	case view.MailApproval.MailListID == nil:
		count = int64(len(view.RecipientEmails))
	case view.InternalMailList:
		n, err := h.db.CountRecipientsByMailingListId(ctx, *view.MailApproval.MailListID)
		if err != nil {
			log.Warn().Err(err).Str("approval_id", view.MailApproval.ID.String()).Msg("could not count an approval request's list")
			return nil
		}
		count = n
	default:
		ctx, cancel := context.WithTimeout(ctx, approvalKeycloakBudget)
		defer cancel()
		members, err := h.kc.GetGroupMembers(ctx, view.MailApproval.MailListID.String())
		if err != nil {
			log.Warn().Err(err).Str("approval_id", view.MailApproval.ID.String()).Msg("could not count an approval request's Keycloak group")
			return nil
		}
		count = int64(len(membersWithAddress(members)))
	}
	return &count
}
