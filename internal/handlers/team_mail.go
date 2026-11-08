package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
)

// Team mail (ADR-0067 addendum of 2026-11-08; hub tickets ekip-postasi 03
// and 04): a Leader mails their own Team, or Sub-teams below it, through
// Mail onayı, YK approving. The rules:
//
//   - core says which Teams a person leads (teams.Reader); SkyMail reads
//     neither Keycloak nor the token for it, and asks again on submit,
//     resubmit and at the approval. Core not answering is no Team mail.
//   - A request names the Teams in team_ids: any of those the submitter
//     leads, a Leader group or a Team they do not lead never. A picked
//     Sub-team of a picked Team is dropped; the rest, in path order, are each
//     their own send with their own category, team:<id>:duyuru, always
//     duyuru. No list read role is needed: the Leader sees counts, never
//     addresses.
//   - At the approval a Team whose leadership the submitter lost is dropped,
//     recorded and told; with none left the request is rejected with the
//     reason. Core not answering then leaves the request as it is (503).
//   - One mail per person: someone in two picked Teams gets it from the
//     first in path order whose category they have not turned off, decided
//     when the sends are queued.
//   - A Leader without skymail:access reaches only Team mail's routes
//     (middlewares.RequireAccessOrTeamLeader) and writes a free announcement
//     (FreeTemplateKey), no other template.

// FreeTemplateKey is the free announcement's frame, the only template a
// Leader without skymail:access submits.
const FreeTemplateKey = "free.basic"

// maxTeamsPerRequest is the most Teams one request names.
const maxTeamsPerRequest = 50

var (
	ErrTeamMailOff = apperrors.New("team_mail.off",
		"Team mail is off (SKYMAIL_TEAM_SEND).", fiber.StatusNotFound)
	errTeamNotLed = apperrors.New("team_mail.not_leader",
		"You do not lead a Team you picked (core's answer now). params.team_ids names them.", fiber.StatusForbidden)
	errTeamFreeAnnouncementOnly = apperrors.New("team_mail.free_announcement_only",
		"Without SkyMail's roles a Leader sends a free announcement (free.basic) to their Teams, no other template.", fiber.StatusForbidden)
	errTeamPurposeFixed = apperrors.New("team_mail.purpose_fixed",
		"Team mail is always duyuru, each Team's send with its own category team:<id>:duyuru: send no purpose other than duyuru and no other category.", fiber.StatusBadRequest)
	errTeamMailOffAtApproval = apperrors.New("mail_approval.team_mail_off",
		"Team mail is off (SKYMAIL_TEAM_SEND, or SKYMAIL_MAIL_PURPOSE off): the request stays as it is and is not sent.", fiber.StatusConflict)
	errTeamLeadershipLost = apperrors.New("mail_approval.team_leadership_lost",
		"The submitter no longer leads any of the request's Teams (core's answer now): it was rejected with that reason and nothing was sent.", fiber.StatusConflict)
)

// teamCategory is a Team's own announcement category.
func teamCategory(id uuid.UUID) string {
	return "team:" + id.String() + ":duyuru"
}

// teamsNamedIn says a body that has not been checked yet names Teams.
func teamsNamedIn(body []byte) bool {
	var peek struct {
		TeamIDs []uuid.UUID `json:"team_ids"`
	}
	return json.Unmarshal(body, &peek) == nil && len(peek.TeamIDs) > 0
}

// errAccessRequired refuses a Leader without skymail:access anything but
// Team mail.
func errAccessRequired() error {
	return apperrors.ErrForbidden.WithParams(map[string]interface{}{"missing_roles": []string{"skymail:access"}})
}

// pickTeams is the Teams a request goes to: each of ids must be one the
// person leads (led, core's answer), and a picked Sub-team of another picked
// Team is dropped. In path order.
func pickTeams(led []teams.Team, ids []uuid.UUID) ([]teams.Team, error) {
	byID := make(map[uuid.UUID]teams.Team, len(led))
	for _, team := range led {
		byID[team.ID] = team
	}
	var picked []teams.Team
	var notLed []uuid.UUID
	for _, id := range ids {
		team, ok := byID[id]
		if !ok || teams.LeaderGroupPath(team.Path) {
			notLed = append(notLed, id)
			continue
		}
		picked = append(picked, team)
	}
	if len(notLed) > 0 {
		return nil, errTeamNotLed.WithParams(map[string]interface{}{"team_ids": notLed})
	}
	inPathOrder(picked)
	var kept []teams.Team
	for _, team := range picked {
		covered := false
		for _, above := range kept {
			if teams.Within(team.Path, above.Path) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, team)
		}
	}
	return kept, nil
}

func inPathOrder(list []teams.Team) {
	slices.SortStableFunc(list, func(a, b teams.Team) int { return strings.Compare(a.Path, b.Path) })
}

// teamSend checks a Team mail submission or resubmission and makes it what
// is written: the switch is on, a Leader without skymail:access sends the
// free announcement, core says now that the submitter leads every Team
// picked, and the purpose is duyuru with the first Team's category (each
// send gets its own at the approval). Its group is the first Team's.
func (h *mailApprovalHandlerImpl) teamSend(ctx context.Context, caller approvalCaller, send approvalSend) (approvalSend, error) {
	if h.teams == nil || !h.mailer.PurposeAvailable() {
		return send, ErrTeamMailOff
	}
	// The free announcement needs no template read role in Team mail,
	// whoever sends it; a Leader without skymail:access sends nothing else.
	template, err := h.db.GetTemplateById(ctx, send.templateID)
	if isNotFound(err) {
		return send, errApprovalTemplateUnavailable
	}
	if err != nil {
		return send, err
	}
	send.freeAnnouncement = template.Key != nil && *template.Key == FreeTemplateKey
	if caller.teamSeat && !send.freeAnnouncement {
		return send, errTeamFreeAnnouncementOnly
	}
	led, err := h.teams.LedNow(ctx, caller.sub)
	if err != nil {
		log.Warn().Err(err).Str("event", "team_mail_leadership_unavailable").Msg("Team mail: core did not say which Teams the submitter leads")
		return send, teams.ErrLeadershipUnavailable
	}
	picked, err := pickTeams(led, send.teamIDs)
	if err != nil {
		return send, err
	}
	send.teamIDs = make([]uuid.UUID, len(picked))
	for i, team := range picked {
		send.teamIDs[i] = team.ID
	}
	first := send.teamIDs[0]
	send.mailListID = &first
	send.purpose, err = h.teamPurpose(send)
	return send, err
}

// teamPurpose is Team mail's purpose: duyuru, the first Team's category,
// never promotional. The body may name duyuru, and a category only when it
// is one of the picked Teams' own; with every duyuru send obeying the
// preferences (SKYMAIL_BULK_PREFERENCE) commercial_content_absent is
// required, as for any duyuru send.
func (h *mailApprovalHandlerImpl) teamPurpose(send approvalSend) (mailer.SendPurpose, error) {
	asked := send.askedPurpose
	if asked.Purpose != nil && *asked.Purpose != mailer.PurposeDuyuru {
		return mailer.SendPurpose{}, errTeamPurposeFixed
	}
	if asked.Category != nil {
		category, ok := canonicalCategory(*asked.Category)
		own := false
		for _, id := range send.teamIDs {
			own = own || (ok && category == teamCategory(id))
		}
		if !own {
			return mailer.SendPurpose{}, errTeamPurposeFixed
		}
	}
	duyuru, category := mailer.PurposeDuyuru, teamCategory(send.teamIDs[0])
	return choosePurpose(true, false, false, h.mailer.PreferenceAvailable(),
		requests.SendPurpose{Purpose: &duyuru, Category: &category, CommercialContentAbsent: asked.CommercialContentAbsent}, targetAudience)
}

// teamPlan is what an approval of Team mail found out before the request's
// lock: which Teams the submitter still leads, and each kept Team's send.
type teamPlan struct {
	// The request's team_ids as read.
	asked []uuid.UUID
	// The Teams gone from Keycloak since they were picked.
	gone []uuid.UUID
	// The Teams whose leadership the submitter lost, and their names
	// (Keycloak's, best effort) for the record.
	lost      []uuid.UUID
	lostNames []string
	sends     []teamSendPlan
}

// teamSendPlan is one Team's send: its purpose, its recipients after one
// mail per person, and what core said about them.
type teamSendPlan struct {
	id         uuid.UUID
	purpose    mailer.SendPurpose
	recipients []mailer.RecipientInfo
	verdicts   *mailer.PreferenceVerdicts
	tracking   *mailer.TrackingPreferences
}

// dropped says the approval drops a Team: led no more, or gone.
func (p *teamPlan) dropped() bool { return len(p.lost)+len(p.gone) > 0 }

func (p *teamPlan) kept() []uuid.UUID {
	ids := make([]uuid.UUID, len(p.sends))
	for i, s := range p.sends {
		ids[i] = s.id
	}
	return ids
}

// planTeams asks, before the lock, everything an approval of Team mail
// needs: core's leadership now, each kept Team's members (Keycloak), core's
// preferences in each Team's category, and whom core lets be tracked.
func (h *mailApprovalHandlerImpl) planTeams(ctx context.Context, view database.GetMailApprovalRow, tracked bool) (*teamPlan, error) {
	a := view.MailApproval
	if h.teams == nil || !h.mailer.PurposeAvailable() {
		return nil, errTeamMailOffAtApproval
	}
	led, err := h.teams.LedNow(ctx, a.SubmitterSub)
	if err != nil {
		log.Warn().Err(err).Str("event", "team_mail_leadership_unavailable").Str("approval_id", a.ID.String()).
			Msg("Team mail: core did not say which Teams the submitter leads at the approval: the request stays as it is")
		return nil, teams.ErrLeadershipUnavailable
	}
	leads := make(map[uuid.UUID]bool, len(led))
	for _, team := range led {
		leads[team.ID] = !teams.LeaderGroupPath(team.Path)
	}
	plan := &teamPlan{asked: a.TeamIds}
	for _, id := range a.TeamIds {
		if !leads[id] {
			plan.lost = append(plan.lost, id)
			plan.lostNames = append(plan.lostNames, h.groupName(ctx, id))
			continue
		}
		group, err := h.groupRecipients(ctx, id)
		if err != nil {
			return nil, err
		}
		if group.groupMembers == nil {
			// Gone from Keycloak since it was picked: dropped, and said so.
			plan.gone = append(plan.gone, id)
			continue
		}
		purpose := h.queuedPurpose(a)
		category := teamCategory(id)
		purpose.Category, purpose.OwnEventStatement = &category, nil
		verdicts, err := h.mailer.ResolvePreferences(ctx, purpose, group.groupMembers)
		if err != nil {
			return nil, err
		}
		plan.sends = append(plan.sends, teamSendPlan{id: id, purpose: purpose, recipients: group.groupMembers, verdicts: verdicts})
	}
	onePerPerson(plan.sends)
	if tracked {
		for i := range plan.sends {
			if len(plan.sends[i].recipients) > 0 {
				id := plan.sends[i].id
				plan.sends[i].tracking = h.mailer.ResolveTracking(ctx, a.TemplateID, &id, plan.sends[i].recipients)
			}
		}
	}
	return plan, nil
}

// groupName is a Keycloak group's name for a record, or its id.
func (h *mailApprovalHandlerImpl) groupName(ctx context.Context, id uuid.UUID) string {
	ctx, cancel := context.WithTimeout(ctx, keycloakGroupNameBudget)
	defer cancel()
	if group, err := h.kc.GetGroup(ctx, id.String()); err == nil && group != nil && group.Name != nil {
		return *group.Name
	}
	return id.String()
}

// personKey is how one mail per person tells people apart: by Keycloak
// subject, else by address.
func personKey(r mailer.RecipientInfo) string {
	if r.Sub != nil {
		return "sub:" + r.Sub.String()
	}
	return "email:" + strings.ToLower(strings.TrimSpace(r.Email))
}

// onePerPerson leaves each person in one Team's send: the first, in path
// order, whose category core did not say they turned off. Someone who turned
// off every one of their picked Teams stays in the first, where the send
// records them skipped and mails them nothing.
func onePerPerson(sends []teamSendPlan) {
	given := map[string]bool{}
	first := map[string]int{}
	firstAs := map[string]mailer.RecipientInfo{}
	var order []string
	kept := make([][]mailer.RecipientInfo, len(sends))
	for i, send := range sends {
		for _, r := range send.recipients {
			key := personKey(r)
			if given[key] {
				continue
			}
			if _, seen := first[key]; !seen {
				first[key], firstAs[key] = i, r
				order = append(order, key)
			}
			if send.verdicts.Allows(r.Sub, r.Email) {
				given[key] = true
				kept[i] = append(kept[i], r)
			}
		}
	}
	for _, key := range order {
		if !given[key] {
			kept[first[key]] = append(kept[first[key]], firstAs[key])
		}
	}
	for i := range sends {
		if kept[i] == nil {
			kept[i] = []mailer.RecipientInfo{}
		}
		sends[i].recipients = kept[i]
	}
}

func sameTeams(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// applyTeamPlan holds a Team mail request, locked, to its plan before it is
// sent: none of its Teams led any more rejects it with the reason (lost
// true); some lost are dropped, recorded as an edit with the reason. The
// request's Teams changing since the plan was made is a conflict.
func (h *mailApprovalHandlerImpl) applyTeamPlan(ctx context.Context, q *database.Queries, a database.MailApproval, caller approvalCaller, plan *teamPlan) (database.MailApproval, approvalNotice, bool, error) {
	if !sameTeams(a.TeamIds, plan.asked) {
		return a, approvalNotice{}, false, errApprovalChanged
	}
	if !plan.dropped() {
		return a, approvalNotice{}, false, nil
	}
	if len(plan.sends) == 0 && len(plan.lost) == 0 {
		// Every Team gone from Keycloak: as a send to a gone group.
		return a, approvalNotice{}, false, atSend(errApprovalAudienceUnavailable)
	}
	if len(plan.sends) == 0 {
		reason := "SkyMail: gönderen artık seçilen ekiplerin hiçbirinin lideri değil (core'un onay anındaki cevabı): " +
			strings.Join(plan.lostNames, ", ") + "."
		if len(plan.gone) > 0 {
			reason += " " + goneTeamsNote(plan)
		}
		reason += " İstek gönderilmeden reddedildi."
		if err := h.decide(ctx, q, a, caller, approvalStep{
			state: database.MailApprovalStateRejected, kind: database.MailApprovalEventKindRejected, note: reason,
		}); err != nil {
			return a, approvalNotice{}, false, err
		}
		notice := resolvedNotice(decisionRejected, caller, nil, reason)
		notice.decidedBy = "SkyMail"
		return a, notice, true, nil
	}
	kept := plan.kept()
	before, _ := json.Marshal(map[string]interface{}{"team_ids": a.TeamIds})
	after, _ := json.Marshal(map[string]interface{}{"team_ids": kept})
	recorded, _ := json.Marshal([]MailApprovalChange{{Field: "audience", Before: before, After: after}})
	a, err := q.SetMailApprovalTeams(ctx, database.SetMailApprovalTeamsParams{TeamIds: kept, At: h.now(), ID: a.ID})
	if err != nil {
		return a, approvalNotice{}, false, err
	}
	note := droppedTeamsNote(plan)
	_, err = q.RecordMailApprovalEvent(ctx, database.RecordMailApprovalEventParams{
		ApprovalID: a.ID,
		Kind:       database.MailApprovalEventKindEdited,
		ActorSub:   &caller.sub,
		ActorName:  caller.name,
		Note:       &note,
		Changes:    recorded,
		At:         h.now(),
	})
	return a, approvalNotice{}, false, err
}

// droppedTeamsNote says which Teams an approval dropped, and why.
func droppedTeamsNote(plan *teamPlan) string {
	var parts []string
	if len(plan.lost) > 0 {
		parts = append(parts, "SkyMail: gönderenin liderliği bittiği için bu ekipler gönderimden çıkarıldı (core'un onay anındaki cevabı): "+
			strings.Join(plan.lostNames, ", ")+".")
	}
	if len(plan.gone) > 0 {
		parts = append(parts, goneTeamsNote(plan))
	}
	return strings.Join(parts, " ")
}

// goneTeamsNote names the Teams gone from Keycloak since they were picked.
func goneTeamsNote(plan *teamPlan) string {
	ids := make([]string, len(plan.gone))
	for i, id := range plan.gone {
		ids[i] = id.String()
	}
	return "SkyMail: Keycloak'ta artık bulunmadığı için bu ekipler gönderimden çıkarıldı: " + strings.Join(ids, ", ") + "."
}

// queueToTeams queues a Team mail request, locked, as one send per Team in
// path order, each with its own category and its own people (one mail per
// person, planTeams). A Team left with nobody is not sent; with nobody at
// all the request is refused as empty.
func queueToTeams(ctx context.Context, h *mailApprovalHandlerImpl, q *database.Queries, a database.MailApproval, plan *teamPlan) ([]uuid.UUID, error) {
	if plan == nil || !sameTeams(a.TeamIds, plan.kept()) {
		return nil, errApprovalChanged
	}
	var taskIDs []uuid.UUID
	for _, send := range plan.sends {
		if len(send.recipients) == 0 {
			continue
		}
		id := send.id
		taskID, err := h.mailer.Queue(q, mailer.PriorityBulk).WithPreferences(send.verdicts).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy:        a.SubmitterSub,
			TemplateID:    a.TemplateID,
			MailListID:    &id,
			BodyVariables: a.BodyVariables,
			Recipients:    send.recipients,
			Tracking:      a.TrackingRequested,
			Preferences:   send.tracking,
			Purpose:       send.purpose,
		})
		if err != nil {
			return nil, err
		}
		if taskID != uuid.Nil {
			taskIDs = append(taskIDs, taskID)
		}
	}
	if len(taskIDs) == 0 {
		return nil, errApprovalAudienceEmpty
	}
	return taskIDs, nil
}

// teamRecipientCount is how many people a Team mail request would reach now:
// the picked Teams' members with an address, each person once. Nil when
// Keycloak does not say within its budget.
func (h *mailApprovalHandlerImpl) teamRecipientCount(ctx context.Context, ids []uuid.UUID) *int64 {
	ctx, cancel := context.WithTimeout(ctx, approvalKeycloakBudget)
	defer cancel()
	people := map[string]bool{}
	for _, id := range ids {
		members, err := h.kc.GetGroupMembers(ctx, id.String())
		if errors.Is(err, keycloak.ErrGroupNotFound) {
			continue
		}
		if err != nil {
			log.Warn().Err(err).Str("group_id", id.String()).Msg("could not count a Team mail request's Team")
			return nil
		}
		for _, r := range membersWithAddress(members) {
			people[personKey(r)] = true
		}
	}
	count := int64(len(people))
	return &count
}

// TeamAudience is one Team the caller may send Team mail to.
type TeamAudience struct {
	ID          uuid.UUID         `json:"id"`
	Path        string            `json:"path" example:"/UYELER/ARGE/WEBLAB"`
	Code        string            `json:"code" example:"WEBLAB"`
	DisplayName teams.DisplayName `json:"display_name"`
	// The nearest Team above it in this list; null for a top row (a Team whose Leader group the caller is in directly).
	ParentID *uuid.UUID `json:"parent_id"`
	// How many Teams above it are in this list: 0 for a top row.
	Depth int `json:"depth"`
	// The category its send goes with, fixed: team:<id>:duyuru.
	Category string `json:"category" example:"team:6d3c9f8e-2a4b-4c1d-8e7f-1a2b3c4d5e6f:duyuru"`
	// Its members and its Sub-teams' members, Leader groups included, who have an e-mail address, each once: no address, no name. Null when Keycloak did not say in time. A person who turned the Team's category off is counted here and skipped at the send.
	RecipientCount *int64 `json:"recipient_count"`
}

// TeamAudiences is the Teams the caller leads, in path order.
type TeamAudiences struct {
	Teams []TeamAudience `json:"teams"`
}

// TeamMailHandler serves the caller's Team mail audiences.
type TeamMailHandler interface {
	Audiences(c fiber.Ctx) error
}

type teamMailHandlerImpl struct {
	teams teams.Reader
	kc    keycloak.Client
}

// NewTeamMailHandler serves GET /v1/me/team_audiences from core's answer
// (leaders) and Keycloak's group members (counts).
func NewTeamMailHandler(leaders teams.Reader, kc keycloak.Client) TeamMailHandler {
	return &teamMailHandlerImpl{teams: leaders, kc: kc}
}

// teamCountWorkers is how many Teams are counted at once.
const teamCountWorkers = 4

// Audiences godoc
//
//	@Summary		The Teams the caller may send Team mail to
//	@Description	Team mail (ADR-0067 addendum of 2026-11-08): the Teams core says the caller leads — each Team whose Leader group (LIDERLER or KOORDINATORLER) they are directly in, and every Sub-team below it — in path order, the tree in parent_id and depth, each with how many it would reach (its and its Sub-teams' members with an address, Leader groups included, each once; never an address or a name) and its fixed category. Core's answer may be up to a minute old; submitting asks again. A Leader without skymail:access reaches this route; anyone else without it gets 403. An empty list means the caller leads no Team.
//	@Tags			Team mail
//	@Produce		json
//	@Success		200	{object}	handlers.TeamAudiences
//	@Failure		401	{object}	apperrors.AppError	"A token without skymail:access whose aud does not name SkyMail"
//	@Failure		403	{object}	apperrors.AppError	"Neither skymail:access nor a Team the caller leads; auth.person_required for a service account's token without skymail:access"
//	@Failure		404	{object}	apperrors.AppError	"team_mail.off: SKYMAIL_TEAM_SEND is off"
//	@Failure		503	{object}	apperrors.AppError	"team_mail.leadership_unavailable (Retry-After): core did not say which Teams the caller leads"
//	@Router			/me/team_audiences [get]
func (h *teamMailHandlerImpl) Audiences(c fiber.Ctx) error {
	sub, _ := c.Locals("user_id").(string)
	if sub == "" {
		return apperrors.ErrForbidden
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	led, err := h.teams.Led(c.Context(), sub)
	if err != nil {
		log.Warn().Err(err).Str("event", "team_mail_leadership_unavailable").Msg("Team mail: core did not say which Teams the caller leads")
		return teams.ErrLeadershipUnavailable
	}
	answer := TeamAudiences{Teams: make([]TeamAudience, len(led))}
	for i, team := range led {
		row := TeamAudience{ID: team.ID, Path: team.Path, Code: team.Code, DisplayName: team.DisplayName, Category: teamCategory(team.ID)}
		for j := i - 1; j >= 0; j-- {
			if teams.Within(team.Path, led[j].Path) {
				parent := led[j].ID
				row.ParentID = &parent
				row.Depth = answer.Teams[j].Depth + 1
				break
			}
		}
		answer.Teams[i] = row
	}
	h.count(c.Context(), answer.Teams)
	return c.JSON(answer)
}

// count fills each Team's recipient count, a few at once, within one budget.
func (h *teamMailHandlerImpl) count(ctx context.Context, rows []TeamAudience) {
	ctx, cancel := context.WithTimeout(ctx, approvalKeycloakBudget)
	defer cancel()
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(teamCountWorkers, len(rows)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				members, err := h.kc.GetGroupMembers(ctx, rows[i].ID.String())
				if err != nil {
					log.Warn().Err(err).Str("group_id", rows[i].ID.String()).Msg("could not count a Team for Team mail")
					continue
				}
				people := map[string]bool{}
				for _, r := range membersWithAddress(members) {
					people[personKey(r)] = true
				}
				n := int64(len(people))
				rows[i].RecipientCount = &n
			}
		}()
	}
	for i := range rows {
		work <- i
	}
	close(work)
	wg.Wait()
}
