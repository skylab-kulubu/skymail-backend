package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/news"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// POST /v1/news_sends through the chain production registers (userinfo
// stub, skymail:access, skymail:announcements:send), the handler, the real
// database and queue, with an in-memory CMS and core. No dispatcher runs.

const yk = "0c0c0c0c-0000-4000-8000-0000000000aa"

var announcerRoles = []string{"skymail:access", handlers.AnnouncementsSendRole}

type fakeCMS struct {
	mu      sync.Mutex
	records map[string]news.Record
	down    bool
}

func (f *fakeCMS) Get(_ context.Context, slug string) (news.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return news.Record{}, errors.New("cms did not answer")
	}
	record, ok := f.records[slug]
	if !ok {
		return news.Record{}, news.ErrNotFound
	}
	return record, nil
}

type fakeAudiences struct {
	mu      sync.Mutex
	answers map[string]news.Audience
	err     error
	asked   []news.AudienceRequest
	counted int
}

func (f *fakeAudiences) Resolve(_ context.Context, asked news.AudienceRequest) (news.Audience, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, asked)
	if f.err != nil {
		return news.Audience{}, f.err
	}
	return f.answers[news.AudienceKey(asked.Type, asked.Teams)], nil
}

func (f *fakeAudiences) Count(_ context.Context, asked news.AudienceRequest) (news.Counts, news.Skipped, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counted++
	if f.err != nil {
		return news.Counts{}, news.Skipped{}, f.err
	}
	answer := f.answers[news.AudienceKey(asked.Type, asked.Teams)]
	return answer.Counts, answer.Skipped, nil
}

type newsWorld struct {
	t         *testing.T
	store     *database.Store
	app       *fiber.App
	cms       *fakeCMS
	audiences *fakeAudiences
}

func member(sub, email, reason string, categories ...string) news.Person {
	return news.Person{UserID: sub, Email: email, Name: strings.Split(email, "@")[0], Categories: categories, Via: "member",
		Reason: reason, CategoryLabel: "Kulüp geneli · Duyurular", UnsubscribeURL: "https://core.example/v1/communication-preferences/unsubscribe?t=" + sub[len(sub)-2:]}
}

const webTeam = "0b000000-0000-4000-8000-0000000000f1"

func newNewsWorld(t *testing.T, enabled bool) *newsWorld {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	cms := &fakeCMS{records: map[string]news.Record{
		"kayitlar": {Slug: "kayitlar", Version: 3, Title: "Kayıtlar açıldı", Summary: "Bahar dönemi", Kind: news.KindAnnouncement,
			Audience: news.AudiencePublic, Body: `<p>Gel <s>gelme</s></p><script>alert(1)</script><a href="javascript:alert(1)">x</a>`},
		"ozel": {Slug: "ozel", Version: 1, Title: "Özel", Body: "b", Kind: news.KindNews, Audience: news.AudienceTeams, Teams: []string{"/UYELER/ARGE/WEBLAB"}},
	}}
	ayse := member("1a000000-0000-4000-8000-000000000001", "ayse@example.org", "WEBLAB ekibinde olduğun için", "team:"+webTeam+":duyuru")
	mehmet := member("1a000000-0000-4000-8000-000000000002", "mehmet@example.org", "WEBLAB ekibini takip ettiğin için", "team:"+webTeam+":duyuru")
	zeynep := member("1a000000-0000-4000-8000-000000000003", "zeynep@example.org", "SKY LAB üyesi olduğun için", "club:duyuru")
	linkless := member("1a000000-0000-4000-8000-000000000004", "linksiz@example.org", "SKY LAB üyesi olduğun için", "club:duyuru")
	linkless.UnsubscribeURL = ""
	clubAyse, clubMehmet := ayse, mehmet
	clubAyse.Reason, clubMehmet.Reason = "SKY LAB üyesi olduğun için", "SKY LAB üyesi olduğun için"
	audiences := &fakeAudiences{answers: map[string]news.Audience{
		"teams:/UYELER/ARGE/WEBLAB": {People: []news.Person{ayse, mehmet},
			Counts: news.Counts{Members: 1, Followers: 1, Total: 2}, Skipped: news.Skipped{OptedOut: 1}},
		"members": {People: []news.Person{clubAyse, clubMehmet, zeynep, linkless, {UserID: zeynep.UserID, Email: "ZEYNEP@example.org", UnsubscribeURL: zeynep.UnsubscribeURL}},
			Counts: news.Counts{Members: 5, Total: 5}, Skipped: news.Skipped{Unverified: 2, OptedOut: 3}},
	}}

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", newsUserinfo(t), middlewares.AudienceOff)
	api := protectedAPI(app, auth, nil)
	m := mailer.NewMailer(store, mailer.SMTPConfig{}, mailer.SenderOn, mailer.WithSendLanes(mailer.SendLanes{Purpose: true}))
	registerNewsSendRoutes(api, auth, handlers.NewNewsSendHandler(store, m, cms, audiences,
		handlers.NewsSendOptions{Enabled: enabled, ReadMore: news.ReadMoreHome}))
	return &newsWorld{t: t, store: store, app: app, cms: cms, audiences: audiences}
}

// newsUserinfo answers userinfo as claimsUserinfo does, and with the
// bearer's test_email as its verified address and test_name as its name.
func newsUserinfo(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if r.URL.Path != "/protocol/openid-connect/userinfo" || len(parts) != 3 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Sub   string   `json:"sub"`
			Roles []string `json:"test_roles"`
			Email string   `json:"test_email"`
			Name  string   `json:"test_name"`
		}
		_ = json.Unmarshal(payload, &claims)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": claims.Sub, "email": claims.Email, "email_verified": claims.Email != "", "name": claims.Name,
			"resource_access": map[string]any{"skymail": map[string]any{"roles": claims.Roles}},
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// personBearer is a person's token with a verified address.
func personBearer(t *testing.T, sub, email, name string, roles ...string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": sub, "azp": "skymail-ui", "test_roles": roles,
		"test_email": email, "test_name": name}).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (w *newsWorld) seedTemplate() {
	w.t.Helper()
	if _, err := w.store.Conn.Exec(context.Background(), `
		INSERT INTO templates (key, name, system, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('club.news', 'Haber ve duyuru', true, '{{.Subject}}',
		        '<p>{{.AudienceReason}} · {{.CategoryLabel}}</p>{{if .FullBody}}{{safeHTML .BodyHtml}}{{end}}<a href="{{.ReadMoreUrl}}">Devamı</a><a href="{{.UnsubscribeUrl}}">Abonelikten çık</a>',
		        '{{.Title}} {{.UnsubscribeUrl}}', '', false)`); err != nil {
		w.t.Fatal(err)
	}
}

type newsSent struct {
	status int
	raw    []byte
	result handlers.NewsSendResult
	error  struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
}

func (w *newsWorld) post(token string, body any) newsSent {
	w.t.Helper()
	return w.postTo("/v1/news_sends", token, body)
}

func (w *newsWorld) postTo(path, token string, body any) newsSent {
	w.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	var s newsSent
	s.status = response.StatusCode
	s.raw, _ = io.ReadAll(response.Body)
	if s.status == fiber.StatusCreated {
		if err := json.Unmarshal(s.raw, &s.result); err != nil {
			w.t.Fatalf("decode %s: %v", s.raw, err)
		}
	} else {
		_ = json.Unmarshal(s.raw, &s.error)
	}
	return s
}

func (w *newsWorld) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.store.Conn.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func newsSend(slug, audienceType string, teams ...string) map[string]any {
	audience := map[string]any{"type": audienceType}
	if teams != nil {
		audience["teams"] = teams
	}
	return map[string]any{"slug": slug, "audience": audience, "format": "full", "commercial_content_absent": true}
}

// Off, the route answers that it is off; nothing is read.
func TestNewsSendIsOffUnlessSwitchedOn(t *testing.T) {
	w := newNewsWorld(t, false)
	if s := w.post(bearer(t, yk, false, announcerRoles...), newsSend("kayitlar", "members")); s.status != fiber.StatusServiceUnavailable || s.error.Code != "news_send.not_enabled" {
		t.Fatalf("off: %d %s", s.status, s.raw)
	}
	if len(w.audiences.asked) != 0 {
		t.Fatal("core was asked while News sends are off")
	}
}

// A record goes to a team, then to every Member: the second send leaves out
// the team's mailboxes (already_received), each recipient with core's own
// link and words, the record kept as it was, the same audience again 409.
func TestNewsSendGoesToEachMailboxOnce(t *testing.T) {
	w := newNewsWorld(t, true)
	token := bearer(t, yk, false, announcerRoles...)

	if s := w.post(token, newsSend("kayitlar", "members")); s.status != fiber.StatusConflict || s.error.Code != "news_send.template_missing" {
		t.Fatalf("without club.news: %d %s", s.status, s.raw)
	}
	w.seedTemplate()

	team := w.post(token, newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB"))
	if team.status != fiber.StatusCreated {
		t.Fatalf("team send: %d %s", team.status, team.raw)
	}
	if team.result.Queued != 2 || team.result.Counts.AlreadyReceived != 0 || team.result.Counts.Skipped.OptedOut != 1 {
		t.Fatalf("team send result = %+v", team.result)
	}
	if team.result.Purpose == nil || *team.result.Purpose != "duyuru" || team.result.Category == nil || *team.result.Category != "team:"+webTeam+":duyuru" {
		t.Fatalf("team send purpose = %v %v", team.result.Purpose, team.result.Category)
	}
	if team.result.Source != (handlers.NewsSendSource{Ref: "news:kayitlar", Slug: "kayitlar", Version: 3, Kind: "duyuru"}) {
		t.Fatalf("source = %+v", team.result.Source)
	}
	asked := w.audiences.asked[len(w.audiences.asked)-1]
	if asked.NewsKind != "duyuru" || asked.Visibility != "herkes" || asked.Type != "teams" {
		t.Fatalf("core asked %+v", asked)
	}

	var subject, html, text, link, ref string
	var sub *uuid.UUID
	if err := w.store.Conn.QueryRow(context.Background(), `
		SELECT subject, body_html, body, list_unsubscribe_url, source_ref, recipient_sub FROM mail_queue
		WHERE task_id = $1 AND recipient_email = 'mehmet@example.org'`, team.result.ID).Scan(&subject, &html, &text, &link, &ref, &sub); err != nil {
		t.Fatal(err)
	}
	if subject != "Duyuru: Kayıtlar açıldı" || ref != "news:kayitlar" || sub == nil || sub.String() != "1a000000-0000-4000-8000-000000000002" {
		t.Fatalf("row: subject %q ref %q sub %v", subject, ref, sub)
	}
	if !strings.HasSuffix(link, "?t=02") || !strings.Contains(html, `href="https://core.example/v1/communication-preferences/unsubscribe?t=02"`) {
		t.Fatalf("row link %q, html %q", link, html)
	}
	if !strings.Contains(html, "WEBLAB ekibini takip ettiğin için") || !strings.Contains(html, "<s>gelme</s>") ||
		strings.Contains(html, "<script") || strings.Contains(html, "javascript:") || !strings.Contains(html, `href="https://yildizskylab.com/"`) {
		t.Fatalf("row html = %q", html)
	}
	var version int
	var snapshot, counts []byte
	var audience string
	if err := w.store.Conn.QueryRow(context.Background(), `
		SELECT source_version, source_snapshot, source_audience, source_counts FROM mail_tasks WHERE id = $1`, team.result.ID).Scan(&version, &snapshot, &audience, &counts); err != nil {
		t.Fatal(err)
	}
	if version != 3 || audience != "teams:/UYELER/ARGE/WEBLAB" || !strings.Contains(string(snapshot), `"title": "Kayıtlar açıldı"`) || !strings.Contains(string(counts), `"opted_out": 1`) {
		t.Fatalf("task source: v%d %q %s %s", version, audience, snapshot, counts)
	}

	// The record changes on the web; the next send is made of what it is now,
	// the first keeps what it was.
	w.cms.mu.Lock()
	edited := w.cms.records["kayitlar"]
	edited.Version, edited.Title = 4, "Kayıtlar uzatıldı"
	w.cms.records["kayitlar"] = edited
	w.cms.mu.Unlock()

	club := w.post(token, newsSend("kayitlar", "members"))
	if club.status != fiber.StatusCreated {
		t.Fatalf("club send: %d %s", club.status, club.raw)
	}
	if club.result.Queued != 1 || club.result.Counts.AlreadyReceived != 2 || club.result.Counts.Skipped.NoUnsubscribeURL != 1 ||
		club.result.Counts.Skipped.DuplicateEmail != 1 || club.result.Counts.Skipped.Unverified != 2 || club.result.Source.Version != 4 {
		t.Fatalf("club send result = %+v", club.result)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE source_ref = 'news:kayitlar'`); n != 3 {
		t.Fatalf("rows of the record = %d, want 3 (each mailbox once)", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE recipient_email = 'linksiz@example.org'`); n != 0 {
		t.Fatal("a person without an unsubscribe link was queued")
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE task_id = $1 AND subject = 'Duyuru: Kayıtlar açıldı'`, team.result.ID); n != 2 {
		t.Fatal("the first send's rows changed with the record")
	}

	again := w.post(token, newsSend("kayitlar", "members"))
	if again.status != fiber.StatusConflict || again.error.Code != "news_send.already_sent" || again.error.Params["task_id"] != club.result.ID.String() {
		t.Fatalf("again: %d %s", again.status, again.raw)
	}
	reordered := w.post(token, newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB"))
	if reordered.status != fiber.StatusConflict || reordered.error.Params["task_id"] != team.result.ID.String() {
		t.Fatalf("the team again: %d %s", reordered.status, reordered.raw)
	}
}

// What is refused, and that nothing is written when it is.
func TestNewsSendRefusals(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	token := bearer(t, yk, false, announcerRoles...)

	statement := newsSend("kayitlar", "members")
	delete(statement, "commercial_content_absent")
	falseStatement := newsSend("kayitlar", "members")
	falseStatement["commercial_content_absent"] = false
	membersWithTeams := newsSend("kayitlar", "members", "/UYELER/ARGE/WEBLAB")
	teamsWithout := newsSend("kayitlar", "teams")
	badFormat := newsSend("kayitlar", "members")
	badFormat["format"] = "html"

	for name, test := range map[string]struct {
		token  string
		body   map[string]any
		status int
		code   string
	}{
		"no role":            {bearer(t, yk, false, "skymail:access", "skymail:mails:write"), newsSend("kayitlar", "members"), fiber.StatusForbidden, ""},
		"no statement":       {token, statement, fiber.StatusBadRequest, "news_send.commercial_statement_required"},
		"false statement":    {token, falseStatement, fiber.StatusBadRequest, "news_send.commercial_statement_required"},
		"members with teams": {token, membersWithTeams, fiber.StatusBadRequest, "news_send.teams_invalid"},
		"teams without":      {token, teamsWithout, fiber.StatusBadRequest, "news_send.teams_invalid"},
		"format":             {token, badFormat, fiber.StatusBadRequest, "validation.error"},
		"unknown record":     {token, newsSend("yok", "members"), fiber.StatusNotFound, "news_send.not_found"},
		"private record":     {token, newsSend("ozel", "teams", "/UYELER/ARGE/WEBLAB"), fiber.StatusConflict, "news_send.private_record"},
	} {
		if s := w.post(test.token, test.body); s.status != test.status || (test.code != "" && s.error.Code != test.code) {
			t.Errorf("%s: %d %s, want %d %s", name, s.status, s.raw, test.status, test.code)
		}
	}

	w.cms.down = true
	if s := w.post(token, newsSend("kayitlar", "members")); s.status != fiber.StatusServiceUnavailable || s.error.Code != "news_send.cms_unavailable" {
		t.Errorf("cms down: %d %s", s.status, s.raw)
	}
	w.cms.down = false
	w.audiences.err = errors.New("core did not answer")
	if s := w.post(token, newsSend("kayitlar", "members")); s.status != fiber.StatusServiceUnavailable || s.error.Code != "news_send.audience_unavailable" {
		t.Errorf("core down: %d %s", s.status, s.raw)
	}
	w.audiences.err = news.ErrTeamUnknown
	if s := w.post(token, newsSend("kayitlar", "teams", "/UYELER/ARGE")); s.status != fiber.StatusBadRequest || s.error.Code != "news_send.team_unknown" {
		t.Errorf("team unknown: %d %s", s.status, s.raw)
	}
	w.audiences.err = nil
	if s := w.post(token, newsSend("kayitlar", "teams", "/UYELER/BOS")); s.status != fiber.StatusUnprocessableEntity || s.error.Code != "news_send.no_recipients" {
		t.Errorf("nobody: %d %s", s.status, s.raw)
	}
	if n := w.count(`SELECT count(*) FROM mail_tasks`) + w.count(`SELECT count(*) FROM mail_queue`); n != 0 {
		t.Fatalf("a refused send wrote %d rows", n)
	}
}

// The preview is the mail the send would queue, for the caller: the same
// HTML as a queued row but for the recipient's own words and link; nothing
// is written.
func TestNewsSendPreviewIsTheSendsMail(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	preparer := personBearer(t, yk, "hazirlayan@example.org", "Hazırlayan Kişi", "skymail:access", "skymail:mails:write")

	preview := w.postTo("/v1/news_sends/preview", preparer, newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB"))
	if preview.status != fiber.StatusOK {
		t.Fatalf("preview: %d %s", preview.status, preview.raw)
	}
	var rendered mailer.Rendered
	if err := json.Unmarshal(preview.raw, &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Subject != "Duyuru: Kayıtlar açıldı" || !strings.Contains(rendered.HTML, "WEBLAB ekibinde olduğun için · WEBLAB · Duyurular") ||
		!strings.Contains(rendered.HTML, `href="https://my.yildizskylab.com/communication-preferences"`) || strings.Contains(rendered.HTML, "<script") {
		t.Fatalf("preview = %+v", rendered)
	}
	if n := w.count(`SELECT count(*) FROM mail_tasks`); n != 0 {
		t.Fatalf("a preview wrote %d sends", n)
	}

	sent := w.post(bearer(t, yk, false, announcerRoles...), newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB"))
	if sent.status != fiber.StatusCreated {
		t.Fatalf("send: %d %s", sent.status, sent.raw)
	}
	var html string
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT body_html FROM mail_queue WHERE task_id = $1 AND recipient_email = 'ayse@example.org'`,
		sent.result.ID).Scan(&html); err != nil {
		t.Fatal(err)
	}
	ownWords := strings.NewReplacer("WEBLAB ekibinde olduğun için · Kulüp geneli · Duyurular", "",
		"https://core.example/v1/communication-preferences/unsubscribe?t=01", "")
	sampleWords := strings.NewReplacer("WEBLAB ekibinde olduğun için · WEBLAB · Duyurular", "",
		"https://my.yildizskylab.com/communication-preferences", "")
	if ownWords.Replace(html) != sampleWords.Replace(rendered.HTML) {
		t.Fatalf("the preview's HTML differs from the send's beyond the recipient's words:\n%s\n---\n%s", rendered.HTML, html)
	}
}

// A test mail goes to the caller's own address only, opens with [Deneme],
// is no News send (no source, no "already received"), and at most 3 a minute.
func TestNewsSendTestGoesToTheCallerOnly(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	preparer := personBearer(t, yk, "hazirlayan@example.org", "Hazırlayan Kişi", "skymail:access", "skymail:mails:write")

	body := newsSend("kayitlar", "members")
	body["recipient_email"] = "baskasi@example.org"
	body["recipients"] = []string{"baskasi@example.org"}
	for i := 0; i < handlers.NewsTestsPerMinute; i++ {
		test := w.postTo("/v1/news_sends/test", preparer, body)
		if test.status != fiber.StatusCreated {
			t.Fatalf("test %d: %d %s", i+1, test.status, test.raw)
		}
	}
	if s := w.postTo("/v1/news_sends/test", preparer, body); s.status != fiber.StatusTooManyRequests || s.error.Code != "news_send.test_rate_limited" {
		t.Fatalf("the fourth test in a minute: %d %s", s.status, s.raw)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE recipient_email = 'hazirlayan@example.org' AND subject = '[Deneme] Duyuru: Kayıtlar açıldı' AND source_ref IS NULL`); n != handlers.NewsTestsPerMinute {
		t.Fatalf("test rows to the caller = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE recipient_email = 'hazirlayan@example.org'
		AND list_unsubscribe_url IS NULL AND body_html LIKE '%href="https://my.yildizskylab.com/communication-preferences"%'`); n != handlers.NewsTestsPerMinute {
		t.Fatalf("test rows with the sample link in the body and no List-Unsubscribe = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE recipient_email <> 'hazirlayan@example.org'`) + w.count(`SELECT count(*) FROM mail_tasks WHERE source_ref IS NOT NULL`); n != 0 {
		t.Fatalf("a test reached someone else or became a News send: %d", n)
	}
	if s := w.postTo("/v1/news_sends/test", personBearer(t, yk, "", "", "skymail:access", "skymail:mails:write"), body); s.status != fiber.StatusConflict || s.error.Code != "news_send.no_test_address" {
		t.Fatalf("no verified address: %d %s", s.status, s.raw)
	}
	if s := w.postTo("/v1/news_sends/test", personBearer(t, yk, "a@example.org", "", "skymail:access"), body); s.status != fiber.StatusForbidden {
		t.Fatalf("without a role: %d %s", s.status, s.raw)
	}
}

// The live count is the send's numbers without queueing: who would get it,
// who already did, and the send of this audience if there is one.
func TestNewsSendCountIsWhoASendWouldReach(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	preparer := personBearer(t, yk, "hazirlayan@example.org", "", "skymail:access", "skymail:mails:write")
	count := func(audienceType string, teams ...string) handlers.NewsSendCount {
		t.Helper()
		body := newsSend("kayitlar", audienceType, teams...)
		s := w.postTo("/v1/news_sends/count", preparer, map[string]any{"slug": body["slug"], "audience": body["audience"]})
		if s.status != fiber.StatusOK {
			t.Fatalf("count: %d %s", s.status, s.raw)
		}
		var answer handlers.NewsSendCount
		if err := json.Unmarshal(s.raw, &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	before := count("members")
	if before.Counts.Total != 5 || before.Counts.Skipped.OptedOut != 3 || before.Counts.AlreadyReceived != 0 || before.AlreadySentTaskID != nil {
		t.Fatalf("before = %+v", before)
	}
	if w.audiences.counted != 1 || len(w.audiences.asked) != 0 {
		t.Fatalf("core asked %d counts and %d whole audiences, want one count and none", w.audiences.counted, len(w.audiences.asked))
	}
	team := w.post(bearer(t, yk, false, announcerRoles...), newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB"))
	if team.status != fiber.StatusCreated {
		t.Fatalf("team send: %d %s", team.status, team.raw)
	}
	after := count("members")
	if after.Counts.AlreadyReceived != 2 || after.AlreadySentTaskID != nil {
		t.Fatalf("after the team = %+v", after)
	}
	if again := count("teams", "/UYELER/ARGE/WEBLAB"); again.AlreadySentTaskID == nil || *again.AlreadySentTaskID != team.result.ID {
		t.Fatalf("the team again = %+v", again)
	}
	if n := w.count(`SELECT count(*) FROM mail_tasks`); n != 1 {
		t.Fatalf("counting wrote sends: %d", n)
	}
}

// Requests that race: six for the same record and audience make one send
// (the others 409); a record's two audiences at once still reach each mailbox
// once.
func TestNewsSendIsOnceUnderConcurrentRequests(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	token := bearer(t, yk, false, announcerRoles...)

	statuses := make(chan int, 6)
	var wait sync.WaitGroup
	for range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			statuses <- w.post(token, newsSend("kayitlar", "members")).status
		}()
	}
	wait.Wait()
	close(statuses)
	counted := map[int]int{}
	for status := range statuses {
		counted[status]++
	}
	if counted[fiber.StatusCreated] != 1 || counted[fiber.StatusConflict] != 5 {
		t.Fatalf("six racing sends: %v, want one 201 and five 409", counted)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE source_ref = 'news:kayitlar'`); n != 3 {
		t.Fatalf("rows of the record = %d, want 3", n)
	}

	w.cms.mu.Lock()
	w.cms.records["ikinci"] = news.Record{Slug: "ikinci", Version: 1, Title: "İkinci", Body: "b", Kind: news.KindNews, Audience: news.AudiencePublic}
	w.cms.mu.Unlock()
	var both sync.WaitGroup
	for _, body := range []map[string]any{newsSend("ikinci", "members"), newsSend("ikinci", "teams", "/UYELER/ARGE/WEBLAB")} {
		both.Add(1)
		go func() {
			defer both.Done()
			w.post(token, body)
		}()
	}
	both.Wait()
	if n := w.count(`SELECT count(*) - count(DISTINCT lower(recipient_email)) FROM mail_queue WHERE source_ref = 'news:ikinci'`); n != 0 {
		t.Fatalf("%d mailboxes got the record twice", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE source_ref = 'news:ikinci'`); n != 3 {
		t.Fatalf("rows of the second record = %d, want 3", n)
	}
}

// An address SkyMail suppressed is written failed without the record's
// source, so a later send of it asks again; a canonical slug past 200
// characters is refused, not a server error.
func TestNewsSendSuppressedAddressAndLongSlug(t *testing.T) {
	w := newNewsWorld(t, true)
	w.seedTemplate()
	token := bearer(t, yk, false, announcerRoles...)
	if _, err := w.store.Conn.Exec(context.Background(),
		`INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('mehmet@example.org', 'smtp', '550 5.1.1')`); err != nil {
		t.Fatal(err)
	}
	sent := w.post(token, newsSend("kayitlar", "teams", "/UYELER/ARGE/WEBLAB"))
	if sent.status != fiber.StatusCreated || sent.result.Queued != 1 || sent.result.Failed != 1 {
		t.Fatalf("send: %d %s", sent.status, sent.raw)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE recipient_email = 'mehmet@example.org' AND status = 'failed' AND source_ref IS NULL`); n != 1 {
		t.Fatalf("the suppressed address: %d failed rows without a source, want 1", n)
	}

	long := strings.Repeat("a", 201)
	w.cms.mu.Lock()
	w.cms.records["uzun"] = news.Record{Slug: long, Version: 1, Title: "Uzun", Body: "b", Kind: news.KindNews, Audience: news.AudiencePublic}
	w.cms.mu.Unlock()
	if s := w.post(token, newsSend("uzun", "members")); s.status != fiber.StatusBadRequest || s.error.Code != "news_send.slug_too_long" {
		t.Fatalf("a slug past 200: %d %s", s.status, s.raw)
	}
}
