package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/consent"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// Team mail through Mail onayı (ADR-0067 addendum of 2026-11-08; hub tickets
// ekip-postasi 03 and 04) on the real stores and queue, with core's
// leadership, core's preferences and Keycloak stood in for.

// The Teams: WEBLAB and its Sub-team FRONTEND, ALGOLAB, a Team nobody here
// leads, and WEBLAB's Leader group.
var (
	teamWeblab   = uuid.MustParse("10000000-0000-4000-8000-000000000001")
	teamFrontend = uuid.MustParse("10000000-0000-4000-8000-000000000002")
	teamAlgolab  = uuid.MustParse("10000000-0000-4000-8000-000000000003")
	teamOther    = uuid.MustParse("10000000-0000-4000-8000-000000000004")
	teamLeaders  = uuid.MustParse("10000000-0000-4000-8000-000000000005")
)

var (
	weblab   = teams.Team{ID: teamWeblab, Path: "/UYELER/ARGE/WEBLAB", Code: "WEBLAB"}
	frontend = teams.Team{ID: teamFrontend, Path: "/UYELER/ARGE/WEBLAB/FRONTEND", Code: "FRONTEND"}
	algolab  = teams.Team{ID: teamAlgolab, Path: "/UYELER/ARGE/ALGOLAB", Code: "ALGOLAB"}
)

// The people: lider leads WEBLAB (and so FRONTEND) and ALGOLAB without any
// SkyMail role; kaptan leads WEBLAB with skymail:access and
// skymail:templates:read; the members are ayse, mehmet and zeynep.
var (
	lider  = approvalPerson{"20000000-0000-4000-8000-000000000001", "Lider Rolsüz", "lider@yildizskylab.com", []string{}}
	kaptan = approvalPerson{"20000000-0000-4000-8000-000000000002", "Kaptan Rollü", "kaptan@yildizskylab.com",
		[]string{"skymail:access", "skymail:templates:read"}}
	ayse   = approvalPerson{"20000000-0000-4000-8000-000000000003", "Ayşe Kaya", "ayse@example.com", nil}
	mehmet = approvalPerson{"20000000-0000-4000-8000-000000000004", "Mehmet Demir", "mehmet@example.com", nil}
	zeynep = approvalPerson{"20000000-0000-4000-8000-000000000005", "Zeynep Ak", "zeynep@example.com", nil}
)

// teamVerdicts is core's recipients:resolve per category: a sub listed under
// a category turned it off; everyone else is allowed.
type teamVerdicts struct {
	mu  sync.Mutex
	off map[string][]string
}

func (v *teamVerdicts) Resolve(_ context.Context, category string, recipients []consent.Recipient) ([]consent.Result, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	results := make([]consent.Result, len(recipients))
	for i, r := range recipients {
		if slices.Contains(v.off[category], r.Sub) {
			results[i] = consent.Result{Matched: true, Reason: consent.ReasonOptedOut}
			continue
		}
		results[i] = consent.Result{Matched: true, Allowed: true, UnsubscribeURL: "https://api.example.org/v1/communication-preferences/unsubscribe?t=x"}
	}
	return results, nil
}

type teamWorld struct {
	*approvalWorld
	leaders  *chainLeaders
	verdicts *teamVerdicts
}

var teamPeople = map[string]approvalPerson{"lider": lider, "kaptan": kaptan}

func newTeamWorld(t *testing.T) *teamWorld {
	t.Helper()
	ring, err := unsubscribe.NewRing(1, map[byte][]byte{1: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	verdicts := &teamVerdicts{off: map[string][]string{}}
	gate := mailer.PreferenceGate{On: true, Core: verdicts, Links: unsubscribe.Config{On: true, BaseURL: "https://api.example.org/api/skymail", Ring: ring}}
	w := newApprovalWorldWithPool(t, 0, purposesOn(), mailer.WithPreferenceGate(gate))
	leaders := &chainLeaders{led: map[string][]teams.Team{
		lider.sub:  {algolab, weblab, frontend},
		kaptan.sub: {weblab, frontend},
	}}
	w.directory.groups[teamWeblab.String()] = []*gocloak.User{ayse.user(), mehmet.user(), lider.user()}
	w.directory.groups[teamFrontend.String()] = []*gocloak.User{ayse.user()}
	w.directory.groups[teamAlgolab.String()] = []*gocloak.User{ayse.user(), zeynep.user()}
	w.directory.groups[teamOther.String()] = []*gocloak.User{zeynep.user()}
	w.directory.groups[teamLeaders.String()] = []*gocloak.User{lider.user()}

	w.handler = handlers.NewMailApprovalHandler(w.store, w.mail, w.directory, handlers.MailApprovalOptions{
		ClientID: "skymail", UIURL: "https://iletisim.yildizskylab.com/", Now: w.now, Teams: leaders,
	})
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	app.Use(func(c fiber.Ctx) error {
		person, ok := teamPeople[c.Get("X-Test-User")]
		if !ok {
			person, ok = approvalPeople[c.Get("X-Test-User")]
		}
		if !ok {
			t.Fatalf("no test user %q", c.Get("X-Test-User"))
		}
		c.Locals("user_id", person.sub)
		c.Locals("user_name", person.name)
		c.Locals("user_email", person.email)
		c.Locals("roles", person.roles)
		return c.Next()
	})
	// Team mail's chain as registerTeamMailRoutes has it, past the token.
	seat := middlewares.RequireAccessOrTeamLeader(leaders)
	app.Post("/v1/mail_approvals", seat, w.handler.Submit)
	app.Get("/v1/mail_approvals/:id", seat, w.handler.Get)
	app.Post("/v1/mail_approvals/:id/resubmit", seat, w.handler.Resubmit)
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	api := app.Group("/v1")
	api.Use(auth.RequireAnyPermission("skymail:access"))
	registerMailApprovalRoutes(api, auth, w.handler)
	w.app = app
	return &teamWorld{approvalWorld: w, leaders: leaders, verdicts: verdicts}
}

func (w *teamWorld) teamSend(ids ...uuid.UUID) map[string]any {
	send := w.listSend()
	delete(send, "mail_list_id")
	send["team_ids"] = ids
	send["commercial_content_absent"] = true
	return send
}

func (w *teamWorld) submitAs(who string, send map[string]any) (int, approvalAnswer, apiError) {
	w.t.Helper()
	var answer approvalAnswer
	status, raw := w.call(who, fiber.MethodPost, "/v1/mail_approvals", send, &answer)
	var failure apiError
	if status >= 300 {
		_ = json.Unmarshal(raw, &failure)
	}
	return status, answer, failure
}

type teamAnswer struct {
	TeamIDs  []uuid.UUID `json:"team_ids"`
	Purpose  *string     `json:"purpose"`
	Category *string     `json:"category"`
}

func (w *teamWorld) teamsOf(id uuid.UUID) teamAnswer {
	w.t.Helper()
	var answer teamAnswer
	if status, raw := w.call("yusuf", fiber.MethodGet, "/v1/mail_approvals/"+id.String(), nil, &answer); status != fiber.StatusOK {
		w.t.Fatalf("get: %d %s", status, raw)
	}
	return answer
}

// A Leader without any SkyMail role submits the free announcement to Teams
// they lead: a picked Sub-team of a picked Team drops, the rest are in path
// order, the purpose is duyuru with the first Team's category. Another Team,
// a Leader group, another template, another purpose or category, or a list
// is refused; core not answering is 503.
func TestALeaderSubmitsTeamMail(t *testing.T) {
	w := newTeamWorld(t)

	status, answer, failure := w.submitAs("lider", w.teamSend(teamFrontend, teamWeblab, teamAlgolab))
	if status != fiber.StatusCreated {
		t.Fatalf("submit = %d %+v", status, failure)
	}
	got := w.teamsOf(answer.ID)
	if !slices.Equal(got.TeamIDs, []uuid.UUID{teamAlgolab, teamWeblab}) {
		t.Fatalf("team_ids = %v, want ALGOLAB then WEBLAB, FRONTEND dropped", got.TeamIDs)
	}
	if got.Purpose == nil || *got.Purpose != "duyuru" || got.Category == nil || *got.Category != "team:"+teamAlgolab.String()+":duyuru" {
		t.Fatalf("purpose %v category %v", got.Purpose, got.Category)
	}
	if answer.Audience.MailListID == nil || *answer.Audience.MailListID != teamAlgolab {
		t.Fatalf("audience = %+v", answer.Audience)
	}
	if answer.RecipientCount == nil || *answer.RecipientCount != 4 {
		t.Fatalf("recipient_count = %v, want 4 (each person once)", answer.RecipientCount)
	}

	for name, tc := range map[string]struct {
		who    string
		send   map[string]any
		status int
		code   string
	}{
		"a Team not led":    {"lider", w.teamSend(teamOther), fiber.StatusForbidden, "team_mail.not_leader"},
		"a Leader group":    {"lider", w.teamSend(teamLeaders), fiber.StatusForbidden, "team_mail.not_leader"},
		"another template":  {"lider", with(w.teamSend(teamWeblab), map[string]any{"template_id": w.requested.ID}), fiber.StatusForbidden, "team_mail.free_announcement_only"},
		"club category":     {"lider", with(w.teamSend(teamWeblab), map[string]any{"purpose": "duyuru", "category": "club:duyuru"}), fiber.StatusBadRequest, "team_mail.purpose_fixed"},
		"islem":             {"lider", with(w.teamSend(teamWeblab), map[string]any{"purpose": "islem", "transactional_statement": true}), fiber.StatusBadRequest, "team_mail.purpose_fixed"},
		"promotional":       {"lider", with(w.teamSend(teamWeblab), map[string]any{"commercial_content_absent": false}), fiber.StatusBadRequest, "mail_task.commercial_content_not_allowed"},
		"a list":            {"lider", w.listSend(), fiber.StatusForbidden, "server.forbidden"},
		"a team and a list": {"kaptan", with(w.teamSend(teamWeblab), map[string]any{"mail_list_id": w.list.ID}), fiber.StatusBadRequest, "validation.error"},
	} {
		t.Run(name, func(t *testing.T) {
			status, _, failure := w.submitAs(tc.who, tc.send)
			if status != tc.status || failure.Code != tc.code {
				t.Fatalf("= %d %s, want %d %s", status, failure.Code, tc.status, tc.code)
			}
		})
	}

	// Its own category of a picked Team is accepted; a Leader with
	// skymail:templates:read may send another template, without the list
	// read role.
	if status, _, failure := w.submitAs("lider", with(w.teamSend(teamWeblab), map[string]any{"purpose": "duyuru", "category": "team:" + teamWeblab.String() + ":duyuru"})); status != fiber.StatusCreated {
		t.Fatalf("own category = %d %+v", status, failure)
	}
	other := w.seedTemplate("club.news", "Haber", "{{.Subject}}", "<p>{{.Subject}}</p>", "{{.Subject}}", `[]`)
	if status, _, failure := w.submitAs("kaptan", with(w.teamSend(teamFrontend), map[string]any{"template_id": other.ID, "body_variables": map[string]any{"Subject": "x"}})); status != fiber.StatusCreated {
		t.Fatalf("kaptan, another template = %d %+v", status, failure)
	}

	w.leaders.err = errors.New("core down")
	if status, _, failure := w.submitAs("kaptan", w.teamSend(teamWeblab)); status != fiber.StatusServiceUnavailable || failure.Code != "team_mail.leadership_unavailable" {
		t.Fatalf("core down = %d %s", status, failure.Code)
	}
}

// Approved, each Team is its own send with its own category, and a person in
// two picked Teams gets one mail: from the first Team in path order whose
// category they have not turned off.
func TestApprovedTeamMailIsOneSendPerTeamAndOneMailPerPerson(t *testing.T) {
	w := newTeamWorld(t)
	// Ayşe, in ALGOLAB and WEBLAB, turned ALGOLAB's category off.
	w.verdicts.off["team:"+teamAlgolab.String()+":duyuru"] = []string{ayse.sub}

	_, request, _ := w.submitAs("lider", w.teamSend(teamWeblab, teamAlgolab))
	status, approved, failure := w.act("yusuf", request.ID, "approve", nil)
	if status != fiber.StatusOK || approved.State != "approved" || len(approved.TaskIDs) != 2 {
		t.Fatalf("approve = %d %s %v %+v", status, approved.State, approved.TaskIDs, failure)
	}
	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) < 2 {
		t.Fatalf("sends = %+v", sent)
	}
	byTeam := map[uuid.UUID][]string{}
	for _, s := range sent {
		if s.kind == "group" && s.mailListID != nil {
			to := append([]string{}, s.recipients...)
			slices.Sort(to)
			byTeam[*s.mailListID] = to
		}
	}
	// ALGOLAB first: Zeynep, and Ayşe, who turned it off, is not there;
	// WEBLAB: Ayşe (from here), Mehmet, the Leader.
	if !slices.Equal(byTeam[teamAlgolab], []string{"zeynep@example.com"}) ||
		!slices.Equal(byTeam[teamWeblab], []string{"ayse@example.com", "lider@yildizskylab.com", "mehmet@example.com"}) {
		t.Fatalf("by team = %v", byTeam)
	}
	for _, task := range approved.TaskIDs {
		var list uuid.UUID
		var category string
		if err := w.store.Conn.QueryRow(context.Background(), `SELECT mail_list_id, category FROM mail_tasks WHERE id = $1`, task).Scan(&list, &category); err != nil {
			t.Fatal(err)
		}
		if category != "team:"+list.String()+":duyuru" {
			t.Errorf("send to %s has category %s", list, category)
		}
	}

	// Ayşe turned both off: she is in the first Team's send, skipped there.
	w2 := newTeamWorld(t)
	w2.verdicts.off["team:"+teamAlgolab.String()+":duyuru"] = []string{ayse.sub}
	w2.verdicts.off["team:"+teamWeblab.String()+":duyuru"] = []string{ayse.sub}
	_, request, _ = w2.submitAs("lider", w2.teamSend(teamWeblab, teamAlgolab))
	if status, approved, failure = w2.act("yusuf", request.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if st, why, _ := w2.queueRow(approved.TaskIDs[0], "ayse@example.com"); st != "failed" || why != "preference: opted_out" {
		t.Fatalf("Ayşe in ALGOLAB's send: %s %q", st, why)
	}
	var rows int
	if err := w2.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_queue WHERE recipient_email = 'ayse@example.com'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("Ayşe's rows = %d (%v), want 1", rows, err)
	}
}

// Leadership is asked again at the approval: a Team led no more is dropped
// and recorded, the rest go; none led is a rejection with the reason; core
// not answering leaves the request pending.
func TestTeamLeadershipIsCheckedAgainAtTheApproval(t *testing.T) {
	w := newTeamWorld(t)
	_, request, _ := w.submitAs("lider", w.teamSend(teamWeblab, teamAlgolab))

	w.leaders.err = errors.New("core down")
	if status, _, failure := w.act("yusuf", request.ID, "approve", nil); status != fiber.StatusServiceUnavailable || failure.Code != "team_mail.leadership_unavailable" {
		t.Fatalf("core down = %d %s", status, failure.Code)
	}
	if _, after := w.get("yusuf", request.ID); after.State != "pending" {
		t.Fatalf("after core down: %s, want pending", after.State)
	}
	w.leaders.err = nil

	w.leaders.led[lider.sub] = []teams.Team{weblab, frontend}
	status, approved, failure := w.act("yusuf", request.ID, "approve", nil)
	if status != fiber.StatusOK || approved.State != "approved" || len(approved.TaskIDs) != 1 {
		t.Fatalf("approve = %d %s %v %+v", status, approved.State, approved.TaskIDs, failure)
	}
	if got := w.teamsOf(request.ID); !slices.Equal(got.TeamIDs, []uuid.UUID{teamWeblab}) || *got.Category != "team:"+teamWeblab.String()+":duyuru" {
		t.Fatalf("after the drop: %+v", got)
	}
	if approved.kinds() != "submitted,edited,approved" {
		t.Fatalf("history = %s", approved.kinds())
	}
	edit := approved.History[1]
	if edit.Note == nil || len(edit.Changes) != 1 || edit.Changes[0].Field != "audience" {
		t.Fatalf("the drop's record = %+v", edit)
	}

	_, second, _ := w.submitAs("lider", w.teamSend(teamWeblab))
	w.leaders.led[lider.sub] = nil
	status, _, failure = w.act("yusuf", second.ID, "approve", nil)
	if status != fiber.StatusConflict || failure.Code != "mail_approval.team_leadership_lost" {
		t.Fatalf("none led = %d %s", status, failure.Code)
	}
	_, after := w.get("yusuf", second.ID)
	if after.State != "rejected" || after.LastEvent == nil || after.LastEvent.Note == nil || len(after.TaskIDs) != 0 {
		t.Fatalf("after none led: %s %+v", after.State, after.LastEvent)
	}

	// Resubmitting, the Leader who lost the Team cannot.
	if status, raw := w.call("lider", fiber.MethodPost, "/v1/mail_approvals/"+second.ID.String()+"/resubmit", w.teamSend(teamWeblab), nil); status != fiber.StatusForbidden {
		t.Fatalf("resubmit, led no more = %d %s", status, raw)
	}
	w.leaders.led[lider.sub] = []teams.Team{weblab}
	if status, raw := w.call("lider", fiber.MethodPost, "/v1/mail_approvals/"+second.ID.String()+"/resubmit", w.teamSend(teamWeblab), nil); status != fiber.StatusOK {
		t.Fatalf("resubmit, led again = %d %s", status, raw)
	}
}
