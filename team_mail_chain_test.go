package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/teams"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// Team mail's own chain (ADR-0067 addendum of 2026-11-08; hub tickets
// ekip-postasi 03 and 04) through the route table production registers
// (registerPersonAndAPIRoutes), with Keycloak's userinfo and core stood in
// for. The handlers are the real ones on no database: a request that reaches
// one fails there (500) or on its own checks, never with the chain's 401 or
// 403.

// The callers: a Leader with no SkyMail role, a member who leads nothing and
// has no role, a member with skymail:access, and a service account.
var (
	chainLeader  = uuid.MustParse("a1a1a1a1-1111-4111-8111-111111111111")
	chainMember  = uuid.MustParse("b2b2b2b2-2222-4222-8222-222222222222")
	chainStaff   = uuid.MustParse("c3c3c3c3-3333-4333-8333-333333333333")
	chainService = uuid.MustParse("d4d4d4d4-4444-4444-8444-444444444444")
	chainTeam    = uuid.MustParse("e5e5e5e5-5555-4555-8555-555555555555")
	chainCaptain = uuid.MustParse("f6f6f6f6-6666-4666-8666-666666666666")
)

// chainLeaders is core's answer, or its failure.
type chainLeaders struct {
	mu    sync.Mutex
	led   map[string][]teams.Team
	err   error
	calls int
}

func (l *chainLeaders) Led(_ context.Context, sub string) ([]teams.Team, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.led[sub], nil
}

func (l *chainLeaders) LedNow(ctx context.Context, sub string) ([]teams.Team, error) {
	return l.Led(ctx, sub)
}

// chainToken is a token as Keycloak issues it, signed with nothing userinfo
// checks here: the stub accepts it by its text.
func chainToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type chainWorld struct {
	t       *testing.T
	app     *fiber.App
	leaders *chainLeaders
	tokens  map[string]string
}

// newChainWorld serves the route table with Team mail on (leaders set) or
// off (nil), gate in front as production has it.
func newChainWorld(t *testing.T, teamSend bool, gate accessgate.Reader) *chainWorld {
	t.Helper()
	w := &chainWorld{t: t, leaders: &chainLeaders{led: map[string][]teams.Team{
		chainLeader.String():  {{ID: chainTeam, Path: "/UYELER/ARGE/WEBLAB/FRONTEND", Code: "FRONTEND", DisplayName: teams.DisplayName{TR: "Ön Yüz"}}},
		chainCaptain.String(): {{ID: chainTeam, Path: "/UYELER/ARGE/WEBLAB/FRONTEND", Code: "FRONTEND"}},
	}}, tokens: map[string]string{}}
	person := func(sub uuid.UUID, aud ...string) jwt.MapClaims {
		return jwt.MapClaims{"sub": sub.String(), "azp": "skymail-frontend", "aud": aud}
	}
	bodies := map[string]string{}
	add := func(name string, claims jwt.MapClaims, roles ...string) {
		token := chainToken(t, claims)
		w.tokens[name] = token
		raw, _ := json.Marshal(map[string]any{
			"sub": claims["sub"], "name": name, "email": name + "@yildizskylab.com", "email_verified": true,
			"resource_access": map[string]any{"skymail": map[string]any{"roles": roles}},
		})
		bodies[token] = string(raw)
	}
	add("leader", person(chainLeader, "skymail"))
	add("leader-other-aud", person(chainLeader, "account"))
	add("member", person(chainMember, "skymail"))
	add("staff", person(chainStaff, "skymail"), "skymail:access", "skymail:templates:read", "skymail:mails:read")
	add("access-only", person(chainMember, "skymail", "skymail-access-only"), "skymail:access")
	add("captain", person(chainCaptain, "skymail"), "skymail:access")
	add("approving-leader", person(chainLeader, "skymail", "approver"), "skymail:mails:approve")
	add("service", jwt.MapClaims{"sub": chainService.String(), "azp": "core", "client_id": "core", "aud": []string{"skymail"}})

	userinfo := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, ok := bodies[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(userinfo.Close)

	var leaders teams.Reader
	if teamSend {
		leaders = w.leaders
	}
	directory := &approverDirectory{groups: map[string][]*gocloak.User{chainTeam.String(): {elif.user(), baska.user()}}}
	auth := middlewares.NewAuthMiddleware("skymail", userinfo.URL, middlewares.AudienceLog)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	app.Use(recover.New())
	registerPersonAndAPIRoutes(app, auth, gate, leaders, apiHandlers{
		inbox:         handlers.NewInboxHandler(nil),
		emailDelivery: handlers.NewEmailDeliveryHandler(nil),
		team:          handlers.NewTeamMailHandler(leaders, directory),
		template:      handlers.NewTemplateHandler(nil),
		list:          handlers.NewListHandler(nil, directory),
		mail:          handlers.NewMailHandler(nil, nil, directory),
		approval:      handlers.NewMailApprovalHandler(nil, nil, directory, handlers.MailApprovalOptions{ClientID: "skymail"}),
		notification:  handlers.NewNotificationHandler(nil, nil, nil),
		suppression:   handlers.NewSuppressionHandler(nil),
		newsSend:      handlers.NewNewsSendHandler(nil, nil, nil, nil, handlers.NewsSendOptions{}),
	})
	w.app = app
	return w
}

func (w *chainWorld) call(who, method, path, body string) (int, []byte) {
	w.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		request.Header.Set("Authorization", "Bearer "+w.tokens[who])
	}
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

// The routes a Leader without skymail:access reaches, each with a body that
// gets past the handler's own first checks where it has one.
var teamSeatRoutes = map[string]string{
	"GET /v1/me/team_audiences":            "",
	"POST /v1/mail_approvals":              `{"template_id":"` + uuid.NewString() + `","team_ids":["` + chainTeam.String() + `"]}`,
	"GET /v1/mail_approvals":               "",
	"GET /v1/mail_approvals/:id":           "",
	"POST /v1/mail_approvals/:id/resubmit": `{"template_id":"` + uuid.NewString() + `","team_ids":["` + chainTeam.String() + `"]}`,
	"POST /v1/mail_approvals/:id/accept":   "",
	"POST /v1/mail_approvals/:id/decline":  "",
	"GET /v1/templates/by-key/:key":        "",
	"GET /v1/me/inbox":                     "",
	"GET /v1/me/inbox/counts":              "",
	"POST /v1/me/inbox/read-all":           "",
	"POST /v1/me/inbox/seen-all":           "",
	"POST /v1/me/inbox/:id/seen":           "",
	"POST /v1/me/inbox/:id/read":           "",
	"POST /v1/me/inbox/:id/unread":         "",
	"POST /v1/me/inbox/:id/archive":        "",
	"GET /v1/me/email-delivery":            "",
}

func concrete(path string) string {
	path = strings.ReplaceAll(path, ":key", handlers.FreeTemplateKey)
	for _, param := range []string{":id", ":itemId", ":versionId", ":recipientId", ":name"} {
		path = strings.ReplaceAll(path, param, uuid.NewString())
	}
	return path
}

// A Leader without skymail:access reaches Team mail's routes (and, as
// anyone, their own inbox) and no other SkyMail route: every route of the
// table, walked.
func TestALeaderWithoutRolesReachesOnlyTeamMail(t *testing.T) {
	w := newChainWorld(t, true, nil)
	seen := map[string]bool{}
	for _, route := range w.app.GetRoutes(true) {
		if route.Method == fiber.MethodHead || !strings.HasPrefix(route.Path, "/v1") {
			continue
		}
		key := route.Method + " " + strings.TrimSuffix(route.Path, "/")
		if seen[key] {
			continue
		}
		seen[key] = true
		body, allowed := teamSeatRoutes[key]
		if route.Method == fiber.MethodPost && body == "" {
			body = "{}"
		}
		status, raw := w.call("leader", route.Method, concrete(route.Path), body)
		refused := status == fiber.StatusForbidden || status == fiber.StatusUnauthorized
		switch {
		case allowed && refused:
			t.Errorf("%s: %d %s, want the handler reached", key, status, raw)
		case !allowed && status != fiber.StatusForbidden:
			t.Errorf("%s: %d %s, want 403", key, status, raw)
		}
	}
	for key := range teamSeatRoutes {
		if !seen[key] {
			t.Errorf("%s is not served", key)
		}
	}
	if len(seen) < 60 || !seen["GET /v1/mailing_lists"] || !seen["POST /v1/mail_approvals/:id/approve"] {
		t.Fatalf("the walk did not see /v1's routes: %v", seen)
	}
}

// The Leader's list: their Teams with counts, no address. Another template
// than the free announcement is refused.
func TestALeaderWithoutRolesSeesTheirTeamsAndTheFreeTemplateOnly(t *testing.T) {
	w := newChainWorld(t, true, nil)
	status, raw := w.call("leader", fiber.MethodGet, "/v1/me/team_audiences", "")
	var got handlers.TeamAudiences
	if status != fiber.StatusOK || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("team_audiences = %d %s", status, raw)
	}
	if len(got.Teams) != 1 || got.Teams[0].ID != chainTeam || got.Teams[0].RecipientCount == nil || *got.Teams[0].RecipientCount != 2 ||
		got.Teams[0].Category != "team:"+chainTeam.String()+":duyuru" || got.Teams[0].ParentID != nil {
		t.Fatalf("team_audiences = %s", raw)
	}
	if strings.Contains(string(raw), "@") {
		t.Fatalf("an address in the answer: %s", raw)
	}
	if status, raw := w.call("leader", fiber.MethodGet, "/v1/templates/by-key/keycloak.verify-email", ""); status != fiber.StatusForbidden {
		t.Fatalf("another template = %d %s", status, raw)
	}
	// A submission that is not Team mail is refused before anything else.
	status, raw = w.call("leader", fiber.MethodPost, "/v1/mail_approvals", `{"template_id":"`+uuid.NewString()+`","mail_list_id":"`+uuid.NewString()+`"}`)
	if status != fiber.StatusForbidden || !strings.Contains(string(raw), "skymail:access") {
		t.Fatalf("a list submission = %d %s", status, raw)
	}
}

// Leading nothing and having no role is 403; core not answering refuses
// only a caller without skymail:access, who waits for core (503,
// Retry-After). A member with skymail:access never waits for core.
func TestTheSeatIsSkymailAccessOrCoresLeadership(t *testing.T) {
	w := newChainWorld(t, true, nil)
	if status, raw := w.call("member", fiber.MethodGet, "/v1/me/team_audiences", ""); status != fiber.StatusForbidden {
		t.Fatalf("a member leading nothing = %d %s", status, raw)
	}
	if status, raw := w.call("member", fiber.MethodGet, "/v1/mail_approvals", ""); status != fiber.StatusForbidden {
		t.Fatalf("a member leading nothing lists = %d %s", status, raw)
	}

	w.leaders.err = errors.New("core down")
	request := httptest.NewRequest(fiber.MethodGet, "/v1/me/team_audiences", nil)
	request.Header.Set("Authorization", "Bearer "+w.tokens["leader"])
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusServiceUnavailable || response.Header.Get("Retry-After") == "" ||
		!strings.Contains(string(raw), "team_mail.leadership_unavailable") {
		t.Fatalf("core down, the Leader = %d %q %s", response.StatusCode, response.Header.Get("Retry-After"), raw)
	}
	calls := w.leaders.calls
	// skymail:access: past the seat without asking core (the handler then
	// fails on no database, 500, which is not the chain's).
	if status, raw := w.call("staff", fiber.MethodGet, "/v1/mail_approvals", ""); status == fiber.StatusServiceUnavailable || status == fiber.StatusForbidden {
		t.Fatalf("core down, a member with skymail:access = %d %s", status, raw)
	}
	if w.leaders.calls != calls {
		t.Fatal("core was asked about a member with skymail:access")
	}
	// The rest of SkyMail does not depend on core at all.
	if status, raw := w.call("staff", fiber.MethodGet, "/v1/templates", ""); status == fiber.StatusServiceUnavailable || status == fiber.StatusForbidden {
		t.Fatalf("core down, templates = %d %s", status, raw)
	}
}

// Without skymail:access the token must be a person's own, issued for
// SkyMail: another aud is 401, a service account's token 403.
func TestTheSeatWithoutRolesTakesAPersonsTokenForSkyMail(t *testing.T) {
	w := newChainWorld(t, true, nil)
	if status, raw := w.call("leader-other-aud", fiber.MethodGet, "/v1/me/team_audiences", ""); status != fiber.StatusUnauthorized {
		t.Fatalf("another aud = %d %s", status, raw)
	}
	w.leaders.led[chainService.String()] = w.leaders.led[chainLeader.String()]
	if status, raw := w.call("service", fiber.MethodGet, "/v1/me/team_audiences", ""); status != fiber.StatusForbidden || !strings.Contains(string(raw), "auth.person_required") {
		t.Fatalf("a service account = %d %s", status, raw)
	}
	if status, _ := w.call("", fiber.MethodGet, "/v1/me/team_audiences", ""); status != fiber.StatusForbidden {
		t.Fatalf("no token = %d", status)
	}
}

// The account access gate holds Team mail's chain too, before core is
// asked.
func TestTheAccountAccessGateHoldsTeamMail(t *testing.T) {
	for _, decision := range []accessgate.Decision{accessgate.Blocked, accessgate.Unavailable} {
		t.Run(string(decision), func(t *testing.T) {
			gate := &routeTestGate{decision: decision}
			w := newChainWorld(t, true, gate)
			for _, path := range []string{"/v1/me/team_audiences", "/v1/mail_approvals"} {
				status, raw := w.call("leader", fiber.MethodGet, path, "")
				if status != fiber.StatusUnauthorized && status != fiber.StatusServiceUnavailable {
					t.Fatalf("%s: %d %s", path, status, raw)
				}
			}
			if gate.checks == 0 || w.leaders.calls != 0 {
				t.Fatalf("gate checks %d, core asked %d times", gate.checks, w.leaders.calls)
			}
		})
	}
}

// Off: the list says so (404 team_mail.off), and a Leader without roles
// reaches nothing, the approval routes staying on /v1.
func TestTeamMailOff(t *testing.T) {
	w := newChainWorld(t, false, nil)
	status, raw := w.call("leader", fiber.MethodGet, "/v1/me/team_audiences", "")
	if status != fiber.StatusNotFound || !strings.Contains(string(raw), "team_mail.off") {
		t.Fatalf("team_audiences off = %d %s", status, raw)
	}
	if status, raw := w.call("leader", fiber.MethodGet, "/v1/mail_approvals", ""); status != fiber.StatusForbidden {
		t.Fatalf("approvals off = %d %s", status, raw)
	}
	if w.leaders.calls != 0 {
		t.Fatal("core was asked with Team mail off")
	}
}

func TestTeamSendNeedsCorePurposesAndPreferences(t *testing.T) {
	on := mailer.SendLanes{Purpose: true}
	prefs := mailer.PreferenceGate{On: true}
	if err := checkTeamSend(false, "", mailer.SendLanes{}, mailer.PreferenceGate{}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := checkTeamSend(true, "http://core-backend:8080", on, prefs); err != nil {
		t.Fatalf("all set: %v", err)
	}
	for name, err := range map[string]error{
		"no core":        checkTeamSend(true, "", on, prefs),
		"no purposes":    checkTeamSend(true, "http://core-backend:8080", mailer.SendLanes{}, prefs),
		"no preferences": checkTeamSend(true, "http://core-backend:8080", on, mailer.PreferenceGate{}),
	} {
		if err == nil {
			t.Errorf("%s: started", name)
		}
	}
}

// The free announcement's frame is read without skymail:templates:read by a
// Leader, with skymail:access or without; not by anyone else.
func TestTheFreeTemplateIsReadByLeaders(t *testing.T) {
	w := newChainWorld(t, true, nil)
	path := "/v1/templates/by-key/" + handlers.FreeTemplateKey
	for who, reached := range map[string]bool{"leader": true, "captain": true, "staff": true, "access-only": false} {
		status, raw := w.call(who, fiber.MethodGet, path, "")
		refused := status == fiber.StatusForbidden
		if refused == reached {
			t.Errorf("%s: %d %s, reached want %v", who, status, raw, reached)
		}
	}
	if status, _ := w.call("captain", fiber.MethodGet, "/v1/templates/by-key/club.news", ""); status != fiber.StatusForbidden {
		t.Errorf("captain, another template = %d", status)
	}
}

// An approver's role without skymail:access counts for nothing: the Leader
// decides no request.
func TestAnApproverRoleWithoutAccessDecidesNothing(t *testing.T) {
	w := newChainWorld(t, true, nil)
	for _, action := range []string{"approve", "reject", "return"} {
		if status, raw := w.call("approving-leader", fiber.MethodPost, "/v1/mail_approvals/"+uuid.NewString()+"/"+action, "{}"); status != fiber.StatusForbidden {
			t.Errorf("%s = %d %s", action, status, raw)
		}
	}
}
