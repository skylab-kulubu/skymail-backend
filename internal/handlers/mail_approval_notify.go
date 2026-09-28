package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// Whom an action tells, once it is done.
type noticeKind int

const (
	noticeNone noticeKind = iota
	// Every approver, with mail.approval-requested.
	noticeRequested
	// The submitter, with mail.approval-resolved.
	noticeResolved
)

// approvalDecision is how mail.approval-resolved's Decision names what
// happened to a request.
type approvalDecision string

const (
	decisionApproved approvalDecision = "approved"
	decisionRejected approvalDecision = "rejected"
	decisionReturned approvalDecision = "returned"
	decisionExpired  approvalDecision = "expired"
	// The submitter declined a returned edit. Nothing mails it yet: the
	// approver has nothing to do until the submitter resubmits, which tells
	// every approver again.
	decisionDeclined approvalDecision = "declined"
)

// approvalNotice is the mail an action owes once its transaction commits.
type approvalNotice struct {
	kind noticeKind
	// Who acted: the sender of the mail.
	by string
	// mail.approval-resolved's Decision and DecidedBy.
	decision  approvalDecision
	decidedBy string
	// What an approver's edit changed, and the note they or a rejection left.
	changes []MailApprovalChange
	note    string
}

func resolvedNotice(decision approvalDecision, caller approvalCaller, changes []MailApprovalChange, note string) approvalNotice {
	decidedBy := deref(caller.name)
	if decidedBy == "" {
		decidedBy = caller.sub
	}
	return approvalNotice{kind: noticeResolved, by: caller.sub, decision: decision, decidedBy: decidedBy, changes: changes, note: note}
}

// mailTimeZone is where the people reading these mails are.
var mailTimeZone = func() *time.Location {
	if zone, err := time.LoadLocation(summaryTimeZone); err == nil {
		return zone
	}
	// Turkey has kept UTC+3 all year since 2016.
	return time.FixedZone(summaryTimeZone, 3*60*60)
}()

// mailTime is a time as the mails write it: Istanbul's day and hour.
func mailTime(t time.Time) string {
	return t.In(mailTimeZone).Format("02.01.2006 15:04")
}

// decisionNote is mail.approval-resolved's DecisionNote for notice on the
// request as it now stands. The template renders Decision "approved" as an
// approval and anything else as a rejection, so until it learns "returned"
// and "expired" the note is what tells those apart.
func (notice approvalNotice) decisionNote(view database.GetMailApprovalRow) string {
	switch notice.decision {
	case decisionApproved:
		if len(notice.changes) == 0 {
			return strings.TrimSpace(notice.note)
		}
		return withNote("Onaycı göndermeden önce şunları değiştirdi: "+changedVariables(notice.changes)+".", notice.note)
	case decisionReturned:
		return withNote("Onaycı şunları değiştirip gönderimi onayına geri gönderdi: "+changedVariables(notice.changes)+
			". Kabul edersen bu hâliyle gider; etmezsen düzenleyip yeniden sunabilirsin. "+
			mailTime(view.MailApproval.DeadlineAt)+" tarihine kadar karar vermezsen talebin süresi dolar.", notice.note)
	case decisionExpired:
		return "Talebe " + mailTime(view.MailApproval.DeadlineAt) + " tarihine kadar karar verilmediği için süresi doldu; gönderim yapılmadı ve yapılmayacak."
	}
	return strings.TrimSpace(notice.note)
}

func changedVariables(changes []MailApprovalChange) string {
	names := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Name != nil {
			names = append(names, *change.Name)
		}
	}
	return strings.Join(names, ", ")
}

func withNote(text, note string) string {
	note = strings.TrimSpace(note)
	switch {
	case text == "":
		return note
	case note == "":
		return text
	}
	return text + " Not: " + note
}

// deliver sends the mail notice owes, through the mailer like any other
// single send, and says how it went. It is best effort: the action it follows
// is done and stays done whatever happens here.
func (h *mailApprovalHandlerImpl) deliver(ctx context.Context, view database.GetMailApprovalRow, notice approvalNotice) *MailApprovalNotification {
	switch notice.kind {
	case noticeRequested:
		return h.notifyApprovers(ctx, view, notice)
	case noticeResolved:
		return h.notifySubmitter(ctx, view, notice)
	}
	return nil
}

func (h *mailApprovalHandlerImpl) notifyApprovers(ctx context.Context, view database.GetMailApprovalRow, notice approvalNotice) *MailApprovalNotification {
	approvers, problem := h.approvers(ctx)
	if problem != "" {
		return &MailApprovalNotification{TemplateKey: approvalRequestedKey, Problem: &problem}
	}
	requester := deref(view.MailApproval.SubmitterName)
	if requester == "" {
		requester = deref(view.MailApproval.SubmitterEmail)
	}
	link := h.requestURL(view)
	return h.notify(ctx, approvalRequestedKey, notice.by, approvers, map[string]interface{}{
		"RequesterName":  requester,
		"TemplateName":   view.TemplateName,
		"AudienceName":   h.audienceName(ctx, view),
		"AudienceKind":   audienceKind(view),
		"RecipientCount": countText(h.recipientCount(ctx, view)),
		"PreviewUrl":     link + "#preview",
		"ApproveUrl":     link,
	})
}

func (h *mailApprovalHandlerImpl) notifySubmitter(ctx context.Context, view database.GetMailApprovalRow, notice approvalNotice) *MailApprovalNotification {
	if deref(view.MailApproval.SubmitterEmail) == "" {
		problem := "no_address"
		if view.MailApproval.SubmitterEmailUnverified {
			problem = "unverified_address"
		}
		return &MailApprovalNotification{TemplateKey: approvalResolvedKey, Problem: &problem}
	}
	sender := notice.by
	if sender == "" {
		sender = skymailSender
	}
	return h.notify(ctx, approvalResolvedKey, sender, []mailer.RecipientInfo{submitterOf(view.MailApproval)}, map[string]interface{}{
		"TemplateName":   view.TemplateName,
		"AudienceName":   h.audienceName(ctx, view),
		"AudienceKind":   audienceKind(view),
		"RecipientCount": h.resolvedRecipientCount(ctx, view),
		"Decision":       string(notice.decision),
		"DecidedBy":      notice.decidedBy,
		"DecisionNote":   notice.decisionNote(view),
		"DeadlineAt":     mailTime(view.MailApproval.DeadlineAt),
		"RequestUrl":     h.requestURL(view),
	})
}

// audienceKind is the two mails' AudienceKind: who a request goes to, as its
// audience.kind says it — mailing_list (an internal list or a Keycloak
// group), single (one person) or people (several) — so a template can say
// "listesine" or "N kişiye".
func audienceKind(view database.GetMailApprovalRow) string {
	switch {
	case view.MailApproval.MailListID != nil:
		return audienceMailingList
	case len(view.RecipientEmails) == 1:
		return audienceSingle
	}
	return audiencePeople
}

// unknownCount is RecipientCount when it cannot be told: Keycloak did not
// name a group's members in time, or the count could not be read.
const unknownCount = "?"

// countText is a RecipientCount as the mails carry it: the number in digits,
// or unknownCount.
func countText(n *int64) string {
	if n == nil {
		return unknownCount
	}
	return strconv.FormatInt(*n, 10)
}

// resolvedRecipientCount is mail.approval-resolved's RecipientCount. Once a
// request is approved it is how many it was sent to: its people, one send
// each, or the mails its one send to a list queued. Before that it is how many
// it would reach now, as mail.approval-requested says it.
func (h *mailApprovalHandlerImpl) resolvedRecipientCount(ctx context.Context, view database.GetMailApprovalRow) string {
	if view.MailApproval.State != database.MailApprovalStateApproved || view.MailApproval.MailListID == nil || len(view.TaskIds) != 1 {
		return countText(h.recipientCount(ctx, view))
	}
	queued, err := h.db.CountMailQueueItemsByTaskId(ctx, database.CountMailQueueItemsByTaskIdParams{TaskID: view.TaskIds[0]})
	if err != nil {
		log.Warn().Err(err).Str("approval_id", view.MailApproval.ID.String()).Msg("could not count the mails an approved request queued")
		return unknownCount
	}
	return countText(&queued)
}

// requestURL is the request's page in the SkyMail UI.
func (h *mailApprovalHandlerImpl) requestURL(view database.GetMailApprovalRow) string {
	return h.uiURL + "/mail-approvals/show/" + view.MailApproval.ID.String()
}

// approvers are the holders of MailApproverRole with an address, each once —
// the submitter too, when they hold it: they can act on it — or why there are
// none to tell.
func (h *mailApprovalHandlerImpl) approvers(ctx context.Context) ([]mailer.RecipientInfo, string) {
	ctx, cancel := context.WithTimeout(ctx, approvalKeycloakBudget)
	defer cancel()
	users, err := h.kc.ClientRoleMembers(ctx, h.clientID, MailApproverRole)
	if err != nil {
		log.Warn().Err(err).Msg("could not ask Keycloak who approves mail")
		return nil, "approver_lookup_failed"
	}
	seen := map[string]bool{}
	var approvers []mailer.RecipientInfo
	for _, u := range users {
		email := strings.TrimSpace(gocloak.PString(u.Email))
		if email == "" || seen[strings.ToLower(email)] {
			continue
		}
		seen[strings.ToLower(email)] = true
		approvers = append(approvers, mailer.RecipientInfo{FullName: userFullName(u), Email: email})
	}
	if len(approvers) == 0 {
		return nil, "no_approvers"
	}
	return approvers, ""
}

// notify queues the System template key to each recipient, one single send
// each.
func (h *mailApprovalHandlerImpl) notify(ctx context.Context, key, sentBy string, to []mailer.RecipientInfo, variables map[string]interface{}) *MailApprovalNotification {
	notification := &MailApprovalNotification{TemplateKey: key}
	fail := func(problem string) *MailApprovalNotification {
		notification.Problem = &problem
		return notification
	}
	templateKey := key
	template, err := h.db.GetTemplateByKey(ctx, &templateKey)
	if err != nil {
		log.Warn().Err(err).Str("template_key", key).Msg("no System template to notify about a mail approval with")
		return fail("template_unavailable")
	}
	raw, err := json.Marshal(variables)
	if err != nil {
		return fail("enqueue_failed")
	}
	for _, recipient := range to {
		if _, err := h.mailer.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
			SentBy:            sentBy,
			TemplateID:        &template.ID,
			BodyVariables:     raw,
			RecipientFullName: recipient.FullName,
			RecipientEmail:    recipient.Email,
		}); err != nil {
			log.Warn().Err(err).Str("template_key", key).Msg("could not queue a mail approval notification")
			fail("enqueue_failed")
			continue
		}
		notification.Notified++
	}
	return notification
}

// audienceName is who a request goes to, as a notification names it: a list
// by its name, a person by name and address, and several people by the first
// and how many more.
func (h *mailApprovalHandlerImpl) audienceName(ctx context.Context, view database.GetMailApprovalRow) string {
	switch {
	case view.MailApproval.MailListID == nil:
		recipients := recipientsOfView(view)
		if len(recipients) == 0 {
			return ""
		}
		name := recipients[0].Email
		if recipients[0].FullName != "" {
			name = recipients[0].FullName + " <" + name + ">"
		}
		if len(recipients) > 1 {
			name += " ve " + strconv.Itoa(len(recipients)-1) + " kişi daha"
		}
		return name
	case view.InternalMailList:
		return deref(view.MailListName)
	}
	if name := h.approvalGroupNames(ctx, []database.GetMailApprovalRow{view})[*view.MailApproval.MailListID]; name != nil {
		return *name
	}
	return "Keycloak grubu " + view.MailApproval.MailListID.String()
}
