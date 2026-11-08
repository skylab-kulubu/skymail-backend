package handlers

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
)

var (
	weblab   = teams.Team{ID: uuid.MustParse("10000000-0000-4000-8000-000000000001"), Path: "/UYELER/ARGE/WEBLAB"}
	frontend = teams.Team{ID: uuid.MustParse("10000000-0000-4000-8000-000000000002"), Path: "/UYELER/ARGE/WEBLAB/FRONTEND"}
	algolab  = teams.Team{ID: uuid.MustParse("10000000-0000-4000-8000-000000000003"), Path: "/UYELER/ARGE/ALGOLAB"}
	weblab2  = teams.Team{ID: uuid.MustParse("10000000-0000-4000-8000-000000000006"), Path: "/UYELER/ARGE/WEBLAB2"}
	liderler = teams.Team{ID: uuid.MustParse("10000000-0000-4000-8000-000000000005"), Path: "/UYELER/ARGE/WEBLAB/LIDERLER"}
	other    = uuid.MustParse("10000000-0000-4000-8000-000000000004")
)

func ids(list []teams.Team) []uuid.UUID {
	out := make([]uuid.UUID, len(list))
	for i, team := range list {
		out[i] = team.ID
	}
	return out
}

// A picked Sub-team of a picked Team drops; a Team whose path only starts
// like another's does not; the rest are in path order.
func TestPickTeamsDropsCoveredSubTeamsInPathOrder(t *testing.T) {
	led := []teams.Team{algolab, weblab, frontend, weblab2}
	picked, err := pickTeams(led, []uuid.UUID{frontend.ID, weblab2.ID, weblab.ID, algolab.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(picked); !slices.Equal(got, []uuid.UUID{algolab.ID, weblab.ID, weblab2.ID}) {
		t.Fatalf("picked = %v", got)
	}
	picked, err = pickTeams(led, []uuid.UUID{frontend.ID})
	if err != nil || !slices.Equal(ids(picked), []uuid.UUID{frontend.ID}) {
		t.Fatalf("a Sub-team alone = %v, %v", ids(picked), err)
	}
}

// A Team not led, or a Leader group even if core named it, is refused, each
// named.
func TestPickTeamsRefusesWhatIsNotLed(t *testing.T) {
	_, err := pickTeams([]teams.Team{weblab, liderler}, []uuid.UUID{weblab.ID, other, liderler.ID})
	var refused *apperrors.AppError
	if !errors.As(err, &refused) || refused.Code != "team_mail.not_leader" {
		t.Fatalf("err = %v", err)
	}
	if got, _ := refused.Params["team_ids"].([]uuid.UUID); !slices.Equal(got, []uuid.UUID{other, liderler.ID}) {
		t.Fatalf("named %v", refused.Params["team_ids"])
	}
}

func teamMember(sub string, email string) mailer.RecipientInfo {
	id := uuid.MustParse(sub)
	return mailer.RecipientInfo{Email: email, Sub: &id}
}

// One mail per person: the first Team in path order whose category they did
// not turn off; turned off everywhere, they stay in the first, to be
// skipped there. With no verdicts (the check off), the first Team.
func TestOnePerPerson(t *testing.T) {
	ayse := teamMember("20000000-0000-4000-8000-000000000003", "ayse@example.com")
	mehmet := teamMember("20000000-0000-4000-8000-000000000004", "mehmet@example.com")
	zeynep := teamMember("20000000-0000-4000-8000-000000000005", "zeynep@example.com")
	addressOnly := mailer.RecipientInfo{Email: "Misafir@Example.com"}

	sends := []teamSendPlan{
		{id: algolab.ID, recipients: []mailer.RecipientInfo{ayse, zeynep, addressOnly}},
		{id: weblab.ID, recipients: []mailer.RecipientInfo{ayse, mehmet, zeynep, {Email: "misafir@example.com"}}},
	}
	onePerPerson(sends)
	if got := emailsOf(sends[0].recipients); !slices.Equal(got, []string{"ayse@example.com", "zeynep@example.com", "Misafir@Example.com"}) {
		t.Fatalf("first = %v", got)
	}
	if got := emailsOf(sends[1].recipients); !slices.Equal(got, []string{"mehmet@example.com"}) {
		t.Fatalf("second = %v", got)
	}
}

func emailsOf(list []mailer.RecipientInfo) []string {
	out := []string{}
	for _, r := range list {
		out = append(out, r.Email)
	}
	return out
}

func TestTeamCategory(t *testing.T) {
	if got := teamCategory(weblab.ID); got != "team:10000000-0000-4000-8000-000000000001:duyuru" {
		t.Fatal(got)
	}
	if c, ok := canonicalCategory(teamCategory(weblab.ID)); !ok || c != teamCategory(weblab.ID) {
		t.Fatalf("not core's form: %s %v", c, ok)
	}
}

// The body may say duyuru and one of its Teams' own categories; nothing
// else. Promotional is refused; the commercial statement is required while
// the preference check is on.
func TestTeamPurpose(t *testing.T) {
	duyuru, islem := "duyuru", "islem"
	own, club := teamCategory(weblab.ID), "club:duyuru"
	yes, no := true, false
	h := &mailApprovalHandlerImpl{mailer: purposeMailer{preference: true}}
	send := approvalSend{teamIDs: []uuid.UUID{algolab.ID, weblab.ID}}
	for name, tc := range map[string]struct {
		asked requests.SendPurpose
		code  string
	}{
		"nothing but the statement": {requests.SendPurpose{CommercialContentAbsent: &yes}, ""},
		"duyuru, own category":      {requests.SendPurpose{Purpose: &duyuru, Category: &own, CommercialContentAbsent: &yes}, ""},
		"club category":             {requests.SendPurpose{Purpose: &duyuru, Category: &club, CommercialContentAbsent: &yes}, "team_mail.purpose_fixed"},
		"islem":                     {requests.SendPurpose{Purpose: &islem}, "team_mail.purpose_fixed"},
		"promotional":               {requests.SendPurpose{CommercialContentAbsent: &no}, "mail_task.commercial_content_not_allowed"},
		"no statement":              {requests.SendPurpose{}, "mail_task.commercial_statement_required"},
	} {
		t.Run(name, func(t *testing.T) {
			send.askedPurpose = tc.asked
			got, err := h.teamPurpose(send)
			var refused *apperrors.AppError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.code == "":
				if got.Purpose == nil || *got.Purpose != "duyuru" || got.Category == nil || *got.Category != teamCategory(algolab.ID) {
					t.Fatalf("purpose = %+v", got)
				}
			case !errors.As(err, &refused) || refused.Code != tc.code:
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

// purposeMailer is the mailer as teamPurpose asks it.
type purposeMailer struct {
	mailer.Transactional
	preference bool
}

func (m purposeMailer) PreferenceAvailable() bool { return m.preference }
func (m purposeMailer) PurposeAvailable() bool    { return true }
