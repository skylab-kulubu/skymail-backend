package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// The people in the approval tests: a member who cannot send, two approvers,
// and a member who is neither. Each request names who makes it with the
// X-Test-User header; the middleware below stands in for Keycloak's token
// check with that person's subject, name, e-mail and client roles.
type approvalPerson struct {
	sub, name, email string
	roles            []string
}

var (
	elif = approvalPerson{"11111111-1111-4111-8111-111111111111", "Elif Yıldız", "elif@yildizskylab.com",
		[]string{"skymail:access", "skymail:templates:read", "skymail:lists:read"}}
	fatih = approvalPerson{"22222222-2222-4222-8222-222222222222", "Fatih Naz", "fatih@yildizskylab.com",
		[]string{"skymail:access", "skymail:mails:approve"}}
	yusuf = approvalPerson{"33333333-3333-4333-8333-333333333333", "Yusuf Durusoy", "yusuf@yildizskylab.com",
		[]string{"skymail:access", "skymail:mails:approve", "skymail:mails:write"}}
	baska = approvalPerson{"44444444-4444-4444-8444-444444444444", "Başka Üye", "baska@yildizskylab.com",
		[]string{"skymail:access", "skymail:mails:read"}}
)

// A member whose token carries no e-mail address.
var adsiz = approvalPerson{"55555555-5555-4555-8555-555555555555", "Adsız Üye", "", []string{"skymail:access"}}

var approvalPeople = map[string]approvalPerson{"elif": elif, "fatih": fatih, "yusuf": yusuf, "baska": baska, "adsiz": adsiz}

func (p approvalPerson) user() *gocloak.User {
	id, email, first := p.sub, p.email, p.name
	enabled := true
	return &gocloak.User{ID: &id, Email: &email, FirstName: &first, Enabled: &enabled}
}

// approverDirectory is Keycloak as the approval routes ask it: who holds the
// approve role, and a Keycloak group's name and members.
type approverDirectory struct {
	mu        sync.Mutex
	approvers []*gocloak.User
	lookupErr error
	groups    map[string][]*gocloak.User
	roleCalls []string
}

func (d *approverDirectory) ListGroups(context.Context) ([]*gocloak.Group, error) { return nil, nil }

func (d *approverDirectory) GetGroup(_ context.Context, id string) (*gocloak.Group, error) {
	if _, ok := d.groups[id]; !ok {
		return nil, nil
	}
	name := "GECEKODU katılımcıları"
	return &gocloak.Group{ID: &id, Name: &name}, nil
}

func (d *approverDirectory) GetGroupMembers(_ context.Context, id string) ([]*gocloak.User, error) {
	return d.groups[id], nil
}

func (d *approverDirectory) ClientRoleMembers(_ context.Context, clientID, role string) ([]*gocloak.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roleCalls = append(d.roleCalls, clientID+" "+role)
	return d.approvers, d.lookupErr
}

// sentMail is one call into the mailer: which template, to whom, with what.
type sentMail struct {
	kind       string // list, group or single
	templateID uuid.UUID
	sentBy     string
	mailListID *uuid.UUID
	recipients []string
	variables  map[string]any
	taskID     uuid.UUID
}

// recordingMailer is the real mailer — it writes the send and its rendered
// queue rows to Postgres, it only never dispatches them — with every call
// recorded. gate, when set, holds a list send inside the mailer until it is
// closed, so a test can act while an approval is mid-send.
type recordingMailer struct {
	mailer.Mailer
	mu      sync.Mutex
	sent    []sentMail
	gate    chan struct{}
	entered chan struct{}
}

func decodeVariables(raw []byte) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (m *recordingMailer) record(s sentMail) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, s)
}

func (m *recordingMailer) Enqueue(ctx context.Context, arg database.CreateMailTaskParams) (uuid.UUID, error) {
	if m.gate != nil {
		m.entered <- struct{}{}
		<-m.gate
	}
	id, err := m.Mailer.Enqueue(ctx, arg)
	if err == nil {
		m.record(sentMail{kind: "list", templateID: *arg.TemplateID, sentBy: arg.SentBy, mailListID: arg.MailListID,
			variables: decodeVariables(arg.BodyVariables), taskID: id})
	}
	return id, err
}

func (m *recordingMailer) EnqueueSingle(ctx context.Context, arg database.CreateSingleMailTaskParams) (uuid.UUID, error) {
	id, err := m.Mailer.EnqueueSingle(ctx, arg)
	if err == nil {
		m.record(sentMail{kind: "single", templateID: *arg.TemplateID, sentBy: arg.SentBy, recipients: []string{arg.RecipientEmail},
			variables: decodeVariables(arg.BodyVariables), taskID: id})
	}
	return id, err
}

func (m *recordingMailer) EnqueueWithRecipients(ctx context.Context, params mailer.EnqueueWithRecipientsParams) (uuid.UUID, error) {
	id, err := m.Mailer.EnqueueWithRecipients(ctx, params)
	if err == nil {
		var to []string
		for _, r := range params.Recipients {
			to = append(to, r.Email)
		}
		m.record(sentMail{kind: "group", templateID: params.TemplateID, sentBy: params.SentBy, mailListID: params.MailListID,
			recipients: to, variables: decodeVariables(params.BodyVariables), taskID: id})
	}
	return id, err
}

// of is every call made with the template, in order.
func (m *recordingMailer) of(templateID uuid.UUID) []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentMail
	for _, s := range m.sent {
		if s.templateID == templateID {
			out = append(out, s)
		}
	}
	return out
}

// approvalWorld is SkyMail as production serves the approval routes — the real
// routes, permission middleware, handler, error handler, validator, mailer and
// Postgres — with Keycloak and the clock stood in for.
type approvalWorld struct {
	t         *testing.T
	store     *database.Store
	app       *fiber.App
	handler   handlers.MailApprovalHandler
	mail      *recordingMailer
	directory *approverDirectory

	clockMu sync.Mutex
	clock   time.Time

	freeBasic, requested, resolved database.Template
	list                           database.MailingList
	group                          uuid.UUID
}

func (w *approvalWorld) now() time.Time {
	w.clockMu.Lock()
	defer w.clockMu.Unlock()
	return w.clock
}

func (w *approvalWorld) advance(d time.Duration) {
	w.clockMu.Lock()
	defer w.clockMu.Unlock()
	w.clock = w.clock.Add(d)
}

const freeBasicHTML = `<h1>{{.Heading}}</h1><div>{{safeHTML .BodyHtml}}</div><p>{{.FullName}}</p>`

func newApprovalWorld(t *testing.T) *approvalWorld {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(context.Background(), postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	w := &approvalWorld{
		t:     t,
		store: store,
		mail:  &recordingMailer{Mailer: mailer.NewMailer(store, mailer.SMTPConfig{})},
		directory: &approverDirectory{
			approvers: []*gocloak.User{fatih.user(), yusuf.user()},
			groups:    map[string][]*gocloak.User{},
		},
		clock: time.Date(2026, time.September, 23, 10, 0, 0, 0, time.UTC),
	}

	w.freeBasic = w.seedTemplate("free.basic", "Serbest Gönderim", "{{.Subject}}", freeBasicHTML, "{{.BodyHtml}}",
		`[{"name":"BodyHtml","reason":null},{"name":"Subject","reason":"Konu gönderimde yazılır."}]`)
	w.requested = w.seedTemplate("mail.approval-requested", "Mail Onayı · Onayını Bekliyor", "Onayını bekleyen bir gönderim var",
		`<p>{{.RequesterName}} {{.TemplateName}} {{.AudienceName}} {{.RecipientCount}} {{.ApproveUrl}} {{.PreviewUrl}}</p>`, "{{.RequesterName}}", `[]`)
	w.resolved = w.seedTemplate("mail.approval-resolved", "Mail Onayı · Sonuçlandı", "Gönderim talebin sonuçlandı",
		`<p>{{.TemplateName}} {{.AudienceName}} {{.Decision}} {{.DecidedBy}} {{.DecisionNote}}</p>`, "{{.Decision}}", `[]`)

	list, err := store.CreateMailingList(context.Background(), "Tüm üyeler")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ name, email string }{{"Ayşe Kaya", "ayse@example.com"}, {"Mehmet Demir", "mehmet@example.com"}} {
		if _, err := store.AddRecipientToMailingList(context.Background(), database.AddRecipientToMailingListParams{
			MailListID: list.ID, FullName: r.name, Email: r.email,
		}); err != nil {
			t.Fatal(err)
		}
	}
	w.list = list
	w.group = uuid.New()
	w.directory.groups[w.group.String()] = []*gocloak.User{baska.user(), elif.user()}

	w.handler = handlers.NewMailApprovalHandler(store, w.mail, w.directory, handlers.MailApprovalOptions{
		ClientID: "skymail",
		UIURL:    "https://mail.yildizskylab.com/",
		Now:      w.now,
	})
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		person, ok := approvalPeople[c.Get("X-Test-User")]
		if !ok {
			t.Fatalf("no test user %q", c.Get("X-Test-User"))
		}
		c.Locals("user_id", person.sub)
		c.Locals("user_name", person.name)
		if person.email != "" {
			c.Locals("user_email", person.email)
		}
		c.Locals("roles", person.roles)
		return c.Next()
	})
	registerMailApprovalRoutes(api, middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab"), w.handler)
	w.app = app
	return w
}

func (w *approvalWorld) seedTemplate(key, name, subject, html, plain, contract string) database.Template {
	w.t.Helper()
	template, err := w.store.PublishTemplateWrite(context.Background(), database.VersionAuthor{Kind: database.TemplateAuthorKindTemplateSeed},
		func(q *database.Queries) (database.Template, error) {
			return q.UpsertTemplateByKey(context.Background(), database.UpsertTemplateByKeyParams{
				Key: key, Name: name, Subject: subject, HtmlContent: html, PlainTextContent: plain,
				ReactEmailContent: "{}", ContractRequiredVariables: []byte(contract),
			})
		})
	if err != nil {
		w.t.Fatal(err)
	}
	return template
}

// call makes a request as who and decodes the answer into out, when given.
func (w *approvalWorld) call(who, method, path string, body any, out any) (int, []byte) {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("X-Test-User", who)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			w.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	return response.StatusCode, raw
}

// The API's answers, as far as the tests read them.
type approvalNotification struct {
	TemplateKey string  `json:"template_key"`
	Notified    int     `json:"notified"`
	Problem     *string `json:"problem"`
}

type approvalChange struct {
	Field  string          `json:"field"`
	Name   *string         `json:"name"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

type approvalEvent struct {
	Seq   int    `json:"seq"`
	Kind  string `json:"kind"`
	Actor *struct {
		Sub  string  `json:"sub"`
		Name *string `json:"name"`
	} `json:"actor"`
	Note    *string          `json:"note"`
	Changes []approvalChange `json:"changes"`
	TaskID  *uuid.UUID       `json:"task_id"`
	At      time.Time        `json:"at"`
}

type approvalAnswer struct {
	ID        uuid.UUID `json:"id"`
	State     string    `json:"state"`
	Submitter struct {
		Sub   string  `json:"sub"`
		Name  *string `json:"name"`
		Email *string `json:"email"`
	} `json:"submitter"`
	Template struct {
		ID          uuid.UUID `json:"id"`
		VersionID   uuid.UUID `json:"version_id"`
		Name        string    `json:"name"`
		Key         *string   `json:"key"`
		Republished bool      `json:"republished"`
	} `json:"template"`
	Audience struct {
		Kind              string     `json:"kind"`
		MailListID        *uuid.UUID `json:"mail_list_id"`
		Name              *string    `json:"name"`
		Source            *string    `json:"source"`
		RecipientEmail    *string    `json:"recipient_email"`
		RecipientFullName *string    `json:"recipient_full_name"`
	} `json:"audience"`
	BodyVariables  map[string]any `json:"body_variables"`
	SubmittedAt    time.Time      `json:"submitted_at"`
	DeadlineAt     time.Time      `json:"deadline_at"`
	TaskID         *uuid.UUID     `json:"task_id"`
	LastEvent      *approvalEvent `json:"last_event"`
	RecipientCount *int64         `json:"recipient_count"`
	Preview        *struct {
		Subject     string `json:"subject"`
		HTML        string `json:"html"`
		PlainText   string `json:"plain_text"`
		RenderedFor struct {
			FullName string `json:"full_name"`
			Email    string `json:"email"`
		} `json:"rendered_for"`
	} `json:"preview"`
	History      []approvalEvent       `json:"history"`
	Notification *approvalNotification `json:"notification"`
}

type apiError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params"`
}

func (a approvalAnswer) kinds() string {
	var kinds []string
	for _, e := range a.History {
		kinds = append(kinds, e.Kind)
	}
	return strings.Join(kinds, ",")
}

// A GECEKODU announcement to the club-wide list, filled in the way the
// compose form fills the free-form template.
func (w *approvalWorld) listSend() map[string]any {
	return map[string]any{
		"template_id":  w.freeBasic.ID,
		"mail_list_id": w.list.ID,
		"body_variables": map[string]any{
			"Subject":  "GECEKODU başvuruları açıldı",
			"Heading":  "GECEKODU Başvuruları Açıldı",
			"BodyHtml": "<p>Başvurular <strong>5 Nisan</strong>'da kapanıyor.</p>",
		},
	}
}

func (w *approvalWorld) submit(who string, send map[string]any) approvalAnswer {
	w.t.Helper()
	var answer approvalAnswer
	status, raw := w.call(who, fiber.MethodPost, "/v1/mail_approvals", send, &answer)
	if status != fiber.StatusCreated {
		w.t.Fatalf("submit = %d %s", status, raw)
	}
	return answer
}

// act makes a request's action — approve, return, reject, accept, decline or
// resubmit — as who, and reads the answer as a request or as an error.
func (w *approvalWorld) act(who string, id uuid.UUID, action string, body any) (int, approvalAnswer, apiError) {
	w.t.Helper()
	status, raw := w.call(who, fiber.MethodPost, "/v1/mail_approvals/"+id.String()+"/"+action, body, nil)
	var answer approvalAnswer
	var failure apiError
	if status < 300 {
		if err := json.Unmarshal(raw, &answer); err != nil {
			w.t.Fatalf("%s: decode %s: %v", action, raw, err)
		}
	} else {
		_ = json.Unmarshal(raw, &failure)
	}
	return status, answer, failure
}

// get reads a request as who.
func (w *approvalWorld) get(who string, id uuid.UUID) (int, approvalAnswer) {
	w.t.Helper()
	var answer approvalAnswer
	status, _ := w.call(who, fiber.MethodGet, "/v1/mail_approvals/"+id.String(), nil, &answer)
	return status, answer
}

func emails(sent []sentMail) []string {
	var out []string
	for _, s := range sent {
		out = append(out, s.recipients...)
	}
	sort.Strings(out)
	return out
}

func mustStatus(t *testing.T, what string, status, want int, raw []byte) {
	t.Helper()
	if status != want {
		t.Fatalf("%s = %d %s, want %d", what, status, raw, want)
	}
}

// A member who cannot send submits a filled-in send. Nothing goes to its
// recipients; it waits seven days for an approver, and every approver is told
// by the approval-requested System template, with a link to the request.
func TestSubmittingASendQueuesNothingAndTellsTheApprovers(t *testing.T) {
	w := newApprovalWorld(t)

	answer := w.submit("elif", w.listSend())

	if answer.State != "pending" || answer.Submitter.Sub != elif.sub || answer.Template.ID != w.freeBasic.ID ||
		answer.Template.VersionID != *w.freeBasic.PublishedVersionID || answer.Audience.Kind != "mailing_list" ||
		answer.Audience.Name == nil || *answer.Audience.Name != "Tüm üyeler" {
		t.Fatalf("submitted = %+v", answer)
	}
	if !answer.SubmittedAt.Equal(w.now()) || !answer.DeadlineAt.Equal(w.now().Add(7*24*time.Hour)) {
		t.Errorf("submitted %s, deadline %s; want now and seven days on", answer.SubmittedAt, answer.DeadlineAt)
	}
	if answer.kinds() != "submitted" || answer.History[0].Actor == nil || answer.History[0].Actor.Sub != elif.sub {
		t.Errorf("history = %+v", answer.History)
	}
	if sent := w.mail.of(w.freeBasic.ID); len(sent) != 0 {
		t.Fatalf("a submission sent %d mails of the template", len(sent))
	}

	notices := w.mail.of(w.requested.ID)
	if got := strings.Join(emails(notices), ","); got != "fatih@yildizskylab.com,yusuf@yildizskylab.com" {
		t.Fatalf("approval-requested went to %s", got)
	}
	vars := notices[0].variables
	link := "https://mail.yildizskylab.com/mail-approvals/show/" + answer.ID.String()
	if vars["RequesterName"] != "Elif Yıldız" || vars["TemplateName"] != "Serbest Gönderim" || vars["AudienceName"] != "Tüm üyeler" ||
		vars["RecipientCount"] != "2" || vars["ApproveUrl"] != link || vars["PreviewUrl"] != link+"#preview" {
		t.Errorf("approval-requested variables = %v", vars)
	}
	if answer.Notification == nil || answer.Notification.TemplateKey != "mail.approval-requested" ||
		answer.Notification.Notified != 2 || answer.Notification.Problem != nil {
		t.Errorf("notification = %+v", answer.Notification)
	}
}

// A submission is checked as a send would be, and a refused one leaves no
// request behind and tells no one.
func TestSubmissionIsCheckedLikeASend(t *testing.T) {
	w := newApprovalWorld(t)
	ctx := context.Background()

	archived, err := w.store.CreateMailingList(ctx, "Eski liste")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.ArchiveMailingList(ctx, database.ArchiveMailingListParams{ID: archived.ID}); err != nil {
		t.Fatal(err)
	}
	with := func(change func(send map[string]any)) map[string]any {
		send := w.listSend()
		change(send)
		return send
	}
	for name, tc := range map[string]struct {
		send   map[string]any
		status int
		code   string
	}{
		"no template":         {with(func(s map[string]any) { delete(s, "template_id") }), 400, "validation.error"},
		"unknown template":    {with(func(s map[string]any) { s["template_id"] = uuid.New() }), 422, "mail_approval.template_unavailable"},
		"no audience":         {with(func(s map[string]any) { delete(s, "mail_list_id") }), 400, "validation.error"},
		"two audiences":       {with(func(s map[string]any) { s["recipient_email"] = "uye@example.com" }), 400, "validation.error"},
		"malformed recipient": {with(func(s map[string]any) { delete(s, "mail_list_id"); s["recipient_email"] = "uye" }), 400, "validation.error"},
		"archived list":       {with(func(s map[string]any) { s["mail_list_id"] = archived.ID }), 422, "mail_approval.audience_unavailable"},
		"unknown list":        {with(func(s map[string]any) { s["mail_list_id"] = uuid.New() }), 422, "mail_approval.audience_unavailable"},
		"required variable blank": {with(func(s map[string]any) {
			s["body_variables"].(map[string]any)["Subject"] = "  "
		}), 422, "mail_approval.required_variables_missing"},
		"template does not render": {with(func(s map[string]any) {
			// add1 takes a number; a heading is text, so every send of it would fail.
			s["template_id"] = w.seedTemplate("event.notice", "Etkinlik", "{{.Subject}}", "<p>{{add1 .Heading}}</p>{{.BodyHtml}}", "x", `[]`).ID
		}), 422, "mail_approval.unrenderable"},
	} {
		t.Run(name, func(t *testing.T) {
			var failure apiError
			status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", tc.send, &failure)
			if status != tc.status || failure.Code != tc.code {
				t.Fatalf("submit = %d %s, want %d %s", status, raw, tc.status, tc.code)
			}
		})
	}

	var failure apiError
	send := w.listSend()
	delete(send["body_variables"].(map[string]any), "BodyHtml")
	w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, &failure)
	missing, _ := json.Marshal(failure.Params["missing"])
	if string(missing) != `[{"name":"BodyHtml","reason":null,"source":"contract"}]` {
		t.Errorf("missing = %s", missing)
	}

	var count int64
	if err := w.store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_approvals`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || len(w.mail.sent) != 0 {
		t.Fatalf("refused submissions left %d requests and %d mails", count, len(w.mail.sent))
	}
}

// A Keycloak group and a single recipient are audiences too: they are
// checked, and the request shows who it goes to.
func TestSubmittingToAGroupOrOneRecipient(t *testing.T) {
	w := newApprovalWorld(t)

	send := w.listSend()
	send["mail_list_id"] = w.group
	toGroup := w.submit("elif", send)
	if toGroup.Audience.Source == nil || *toGroup.Audience.Source != "keycloak" || toGroup.Audience.Name == nil ||
		*toGroup.Audience.Name != "GECEKODU katılımcıları" {
		t.Errorf("group audience = %+v", toGroup.Audience)
	}
	send["mail_list_id"] = uuid.New()
	if status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", send, nil); status != 422 || !strings.Contains(string(raw), "audience_unavailable") {
		t.Errorf("a group Keycloak does not know = %d %s", status, raw)
	}

	delete(send, "mail_list_id")
	send["recipient_email"] = "konusmaci@example.com"
	send["recipient_full_name"] = "Konuşmacı"
	toOne := w.submit("elif", send)
	if toOne.Audience.Kind != "single" || *toOne.Audience.RecipientEmail != "konusmaci@example.com" {
		t.Errorf("single audience = %+v", toOne.Audience)
	}
	// A single send's preview is that recipient's mail.
	if toOne.Preview == nil || toOne.Preview.RenderedFor.Email != "konusmaci@example.com" ||
		!strings.Contains(toOne.Preview.HTML, "<p>Konuşmacı</p>") {
		t.Errorf("single preview = %+v", toOne.Preview)
	}
}

// With no one else holding the approve role, or Keycloak unable to say who
// does, the submission still stands, and the answer says no one was told.
func TestSubmissionStandsWhenNoApproverCanBeTold(t *testing.T) {
	w := newApprovalWorld(t)

	w.directory.approvers = []*gocloak.User{elif.user()}
	alone := w.submit("elif", w.listSend())
	if alone.State != "pending" || alone.Notification == nil || alone.Notification.Notified != 0 ||
		alone.Notification.Problem == nil || *alone.Notification.Problem != "no_approvers" {
		t.Errorf("with only the submitter approving: %+v", alone.Notification)
	}

	w.directory.lookupErr = context.DeadlineExceeded
	unknown := w.submit("elif", w.listSend())
	if unknown.State != "pending" || unknown.Notification == nil ||
		unknown.Notification.Problem == nil || *unknown.Notification.Problem != "approver_lookup_failed" {
		t.Errorf("with Keycloak failing: %+v", unknown.Notification)
	}
	if got := w.directory.roleCalls; len(got) != 2 || got[0] != "skymail skymail:mails:approve" {
		t.Errorf("role lookups = %v", got)
	}
	if n := len(w.mail.of(w.requested.ID)); n != 0 {
		t.Errorf("%d approval-requested mails, want none", n)
	}
}

// queuedRows is what the mailer queued for a send: one rendered row per
// recipient, by address.
func (w *approvalWorld) queuedRows(taskID uuid.UUID) map[string]database.MailQueue {
	w.t.Helper()
	rows, err := w.store.Conn.Query(context.Background(), `
		SELECT recipient_email, subject, body, body_html FROM mail_queue WHERE task_id = $1`, taskID)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]database.MailQueue{}
	for rows.Next() {
		var row database.MailQueue
		if err := rows.Scan(&row.RecipientEmail, &row.Subject, &row.Body, &row.BodyHtml); err != nil {
			w.t.Fatal(err)
		}
		out[row.RecipientEmail] = row
	}
	return out
}

func (w *approvalWorld) task(taskID uuid.UUID) database.MailTask {
	w.t.Helper()
	var task database.MailTask
	if err := w.store.Conn.QueryRow(context.Background(), `
		SELECT id, sent_by, template_id, mail_list_id, body_variables FROM mail_tasks WHERE id = $1`, taskID).
		Scan(&task.ID, &task.SentBy, &task.TemplateID, &task.MailListID, &task.BodyVariables); err != nil {
		w.t.Fatal(err)
	}
	return task
}

func sameJSON(t *testing.T, got []byte, want any) bool {
	t.Helper()
	var a, b any
	wantRaw, _ := json.Marshal(want)
	if json.Unmarshal(got, &a) != nil || json.Unmarshal(wantRaw, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// An approval without an edit sends the request exactly as submitted, once,
// by its submitter through the send path — every recipient of the list gets
// the mail rendered from those values — and the submitter hears it was
// approved and by whom.
func TestApprovingSendsExactlyWhatWasSubmittedOnce(t *testing.T) {
	w := newApprovalWorld(t)
	send := w.listSend()
	submitted := w.submit("elif", send)
	w.advance(time.Hour)

	status, approved, failure := w.act("fatih", submitted.ID, "approve", map[string]any{"note": "Güzel olmuş."})
	if status != fiber.StatusOK {
		t.Fatalf("approve = %d %+v", status, failure)
	}
	if approved.State != "approved" || approved.TaskID == nil || approved.kinds() != "submitted,approved" {
		t.Fatalf("approved = %s task %v history %s", approved.State, approved.TaskID, approved.kinds())
	}
	last := approved.History[1]
	if last.Actor == nil || last.Actor.Sub != fatih.sub || last.Note == nil || *last.Note != "Güzel olmuş." ||
		last.TaskID == nil || *last.TaskID != *approved.TaskID || !last.At.Equal(w.now()) {
		t.Errorf("approved event = %+v", last)
	}

	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || sent[0].kind != "list" || sent[0].taskID != *approved.TaskID {
		t.Fatalf("sends of the template = %+v, want the one list send", sent)
	}
	task := w.task(*approved.TaskID)
	if task.SentBy != elif.sub || *task.TemplateID != w.freeBasic.ID || *task.MailListID != w.list.ID ||
		!sameJSON(t, task.BodyVariables, send["body_variables"]) {
		t.Errorf("the send = %+v %s", task, task.BodyVariables)
	}
	rows := w.queuedRows(*approved.TaskID)
	ayse := rows["ayse@example.com"]
	if len(rows) != 2 || ayse.Subject != "GECEKODU başvuruları açıldı" || ayse.BodyHtml == nil ||
		!strings.Contains(*ayse.BodyHtml, "<strong>5 Nisan</strong>") || !strings.Contains(*ayse.BodyHtml, "<p>Ayşe Kaya</p>") {
		t.Errorf("queued = %+v", rows)
	}
	// The preview the approver read is the mail each recipient gets, but for
	// who it is addressed to.
	if approved.Preview == nil || strings.Replace(approved.Preview.HTML, "<p>Elif Yıldız</p>", "<p>Ayşe Kaya</p>", 1) != *ayse.BodyHtml {
		t.Errorf("preview %q\nqueued  %q", approved.Preview.HTML, *ayse.BodyHtml)
	}

	notices := w.mail.of(w.resolved.ID)
	if len(notices) != 1 || strings.Join(notices[0].recipients, ",") != elif.email || notices[0].sentBy != fatih.sub {
		t.Fatalf("approval-resolved = %+v", notices)
	}
	vars := notices[0].variables
	if vars["Decision"] != "approved" || vars["DecidedBy"] != "Fatih Naz" || vars["DecisionNote"] != "Güzel olmuş." ||
		vars["TemplateName"] != "Serbest Gönderim" || vars["AudienceName"] != "Tüm üyeler" {
		t.Errorf("approval-resolved variables = %v", vars)
	}
	if approved.Notification == nil || approved.Notification.TemplateKey != "mail.approval-resolved" || approved.Notification.Notified != 1 {
		t.Errorf("notification = %+v", approved.Notification)
	}

	// Approving it again sends nothing; with an edit it is refused.
	status, again, _ := w.act("yusuf", submitted.ID, "approve", nil)
	if status != fiber.StatusOK || again.State != "approved" || *again.TaskID != *approved.TaskID || again.Notification != nil {
		t.Errorf("approving again = %d %+v", status, again)
	}
	status, _, failure = w.act("yusuf", submitted.ID, "approve", map[string]any{"body_variables": map[string]any{"Subject": "x", "BodyHtml": "y"}})
	if status != fiber.StatusConflict || failure.Code != "mail_approval.state_conflict" || failure.Params["state"] != "approved" {
		t.Errorf("approving again with an edit = %d %+v", status, failure)
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 1 {
		t.Fatalf("sends after approving again = %d, want 1", n)
	}
}

// Sends to a Keycloak group and to one recipient go through the same path a
// direct send of each takes.
func TestApprovingSendsToAGroupOrOneRecipient(t *testing.T) {
	w := newApprovalWorld(t)

	send := w.listSend()
	send["mail_list_id"] = w.group
	toGroup := w.submit("elif", send)
	status, approved, failure := w.act("fatih", toGroup.ID, "approve", nil)
	if status != fiber.StatusOK {
		t.Fatalf("approve group send = %d %+v", status, failure)
	}
	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || sent[0].kind != "group" || strings.Join(emails(sent), ",") != "baska@yildizskylab.com,elif@yildizskylab.com" ||
		*sent[0].mailListID != w.group || sent[0].taskID != *approved.TaskID {
		t.Fatalf("group send = %+v", sent)
	}

	delete(send, "mail_list_id")
	send["recipient_email"] = "konusmaci@example.com"
	send["recipient_full_name"] = "Konuşmacı"
	toOne := w.submit("elif", send)
	if status, _, failure := w.act("fatih", toOne.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve single send = %d %+v", status, failure)
	}
	sent = w.mail.of(w.freeBasic.ID)
	if len(sent) != 2 || sent[1].kind != "single" || sent[1].recipients[0] != "konusmaci@example.com" {
		t.Fatalf("single send = %+v", sent)
	}
	if rows := w.queuedRows(sent[1].taskID); !strings.Contains(*rows["konusmaci@example.com"].BodyHtml, "<p>Konuşmacı</p>") {
		t.Errorf("queued single = %+v", rows)
	}
}

// variableChange is the change an event records for one variable.
func variableChange(t *testing.T, changes []approvalChange, name string) (before, after string) {
	t.Helper()
	for _, change := range changes {
		if change.Field == "variable" && change.Name != nil && *change.Name == name {
			return string(change.Before), string(change.After)
		}
	}
	t.Fatalf("no change to %s in %+v", name, changes)
	return "", ""
}

// An approver may edit the variables and send the edit at once: the edit is
// what goes out, the record shows who changed what, and the submitter is told.
func TestApprovingWithAnEditSendsTheEditAndRecordsIt(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())

	edit := map[string]any{
		"Subject":  "GECEKODU başvuruları açıldı!",
		"BodyHtml": "<p>Başvurular <strong>6 Nisan</strong>'da kapanıyor.</p>",
	}
	status, approved, failure := w.act("fatih", submitted.ID, "approve", map[string]any{"body_variables": edit})
	if status != fiber.StatusOK {
		t.Fatalf("approve with an edit = %d %+v", status, failure)
	}
	if approved.State != "approved" || approved.kinds() != "submitted,edited,approved" {
		t.Fatalf("approved = %s %s", approved.State, approved.kinds())
	}
	edited := approved.History[1]
	if edited.Actor == nil || edited.Actor.Sub != fatih.sub || len(edited.Changes) != 3 {
		t.Fatalf("edited event = %+v", edited)
	}
	if before, after := variableChange(t, edited.Changes, "Subject"); before != `"GECEKODU başvuruları açıldı"` || after != `"GECEKODU başvuruları açıldı!"` {
		t.Errorf("Subject changed %s → %s", before, after)
	}
	// The approver's edit is the whole set of variables: one left out is gone.
	if before, after := variableChange(t, edited.Changes, "Heading"); before != `"GECEKODU Başvuruları Açıldı"` || after != "null" {
		t.Errorf("Heading changed %s → %s", before, after)
	}
	variableChange(t, edited.Changes, "BodyHtml")

	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || !sameJSON(t, w.task(sent[0].taskID).BodyVariables, edit) {
		t.Fatalf("sent %+v, want the edit once", sent)
	}
	if !sameJSON(t, mustJSON(t, approved.BodyVariables), edit) {
		t.Errorf("the request holds %v, want the edit", approved.BodyVariables)
	}

	notices := w.mail.of(w.resolved.ID)
	if len(notices) != 1 || notices[0].variables["Decision"] != "approved" ||
		!strings.Contains(notices[0].variables["DecisionNote"].(string), "BodyHtml, Heading, Subject") {
		t.Errorf("approval-resolved = %+v", notices)
	}

	// An edit that changes nothing is no edit.
	again := w.submit("elif", w.listSend())
	status, same, _ := w.act("fatih", again.ID, "approve", map[string]any{"body_variables": w.listSend()["body_variables"]})
	if status != fiber.StatusOK || same.kinds() != "submitted,approved" {
		t.Errorf("approving with the request's own values = %d %s", status, same.kinds())
	}
	// An edit that blanks a Required variable is refused, and nothing is sent.
	third := w.submit("elif", w.listSend())
	status, _, failure = w.act("fatih", third.ID, "approve", map[string]any{"body_variables": map[string]any{"Subject": "", "BodyHtml": "x"}})
	if status != fiber.StatusUnprocessableEntity || failure.Code != "mail_approval.required_variables_missing" {
		t.Errorf("an edit without a subject = %d %+v", status, failure)
	}
	if _, read := w.get("fatih", third.ID); read.State != "pending" || read.kinds() != "submitted" {
		t.Errorf("after a refused edit: %s %s", read.State, read.kinds())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// An approver may instead return the edit to the submitter. Nothing goes out
// until the submitter accepts it; then the edit goes out, once.
func TestAReturnedEditGoesOutWhenTheSubmitterAcceptsIt(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())

	edit := w.listSend()["body_variables"].(map[string]any)
	edit["Heading"] = "GECEKODU 2026 Başvuruları"
	status, returned, failure := w.act("fatih", submitted.ID, "return", map[string]any{"body_variables": edit, "note": "Başlığa yılı ekledim."})
	if status != fiber.StatusOK {
		t.Fatalf("return = %d %+v", status, failure)
	}
	if returned.State != "returned" || returned.kinds() != "submitted,edited,returned" || len(returned.History[1].Changes) != 1 {
		t.Fatalf("returned = %s %s %+v", returned.State, returned.kinds(), returned.History)
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 0 {
		t.Fatalf("a return sent %d mails", n)
	}
	notices := w.mail.of(w.resolved.ID)
	if len(notices) != 1 || notices[0].recipients[0] != elif.email || notices[0].variables["Decision"] != "returned" ||
		!strings.Contains(notices[0].variables["DecisionNote"].(string), "Heading") ||
		!strings.Contains(notices[0].variables["DecisionNote"].(string), "Başlığa yılı ekledim.") {
		t.Fatalf("approval-resolved = %+v", notices)
	}

	// Only the submitter answers a return; an approver cannot approve it now.
	if status, _, failure := w.act("yusuf", submitted.ID, "accept", nil); status != fiber.StatusForbidden || failure.Code != "mail_approval.not_submitter" {
		t.Errorf("an approver accepting = %d %+v", status, failure)
	}
	if status, _, failure := w.act("baska", submitted.ID, "accept", nil); status != fiber.StatusNotFound {
		t.Errorf("someone else accepting = %d %+v", status, failure)
	}
	if status, _, failure := w.act("yusuf", submitted.ID, "approve", nil); status != fiber.StatusConflict || failure.Params["state"] != "returned" {
		t.Errorf("approving a returned request = %d %+v", status, failure)
	}
	// Returning takes an edit.
	other := w.submit("elif", w.listSend())
	if status, _, failure := w.act("fatih", other.ID, "return", map[string]any{"body_variables": w.listSend()["body_variables"]}); status != fiber.StatusUnprocessableEntity || failure.Code != "mail_approval.no_edit" {
		t.Errorf("returning without an edit = %d %+v", status, failure)
	}

	status, accepted, failure := w.act("elif", submitted.ID, "accept", nil)
	if status != fiber.StatusOK {
		t.Fatalf("accept = %d %+v", status, failure)
	}
	if accepted.State != "approved" || accepted.TaskID == nil || accepted.kinds() != "submitted,edited,returned,accepted" ||
		accepted.History[3].Actor.Sub != elif.sub || *accepted.History[3].TaskID != *accepted.TaskID {
		t.Fatalf("accepted = %s %s", accepted.State, accepted.kinds())
	}
	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || sent[0].sentBy != elif.sub || !sameJSON(t, w.task(sent[0].taskID).BodyVariables, edit) {
		t.Fatalf("sent = %+v, want the returned edit once", sent)
	}
	if status, _, _ := w.act("elif", submitted.ID, "accept", nil); status != fiber.StatusConflict {
		t.Errorf("accepting twice = %d", status)
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 1 {
		t.Fatalf("sends after accepting twice = %d", n)
	}
}

// A submitter who declines a returned edit sends nothing and may edit and
// resubmit; the request keeps everything that happened to it.
func TestADeclinedEditCanBeResubmitted(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())
	edit := w.listSend()["body_variables"].(map[string]any)
	edit["Subject"] = "Başvurular açık"
	w.act("fatih", submitted.ID, "return", map[string]any{"body_variables": edit})

	status, declined, failure := w.act("elif", submitted.ID, "decline", map[string]any{"note": "Konu böyle kalsın."})
	if status != fiber.StatusOK || declined.State != "declined" || declined.kinds() != "submitted,edited,returned,declined" ||
		*declined.History[3].Note != "Konu böyle kalsın." {
		t.Fatalf("decline = %d %+v %s", status, failure, declined.kinds())
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 0 {
		t.Fatalf("a decline sent %d mails", n)
	}
	if status, _, _ := w.act("elif", submitted.ID, "accept", nil); status != fiber.StatusConflict {
		t.Errorf("accepting a declined edit = %d", status)
	}

	w.advance(2 * 24 * time.Hour)
	resend := w.listSend()
	resend["body_variables"].(map[string]any)["Heading"] = "GECEKODU'na son çağrı"
	status, resubmitted, failure := w.act("elif", submitted.ID, "resubmit", resend)
	if status != fiber.StatusOK {
		t.Fatalf("resubmit = %d %+v", status, failure)
	}
	if resubmitted.ID != submitted.ID || resubmitted.State != "pending" || resubmitted.kinds() != "submitted,edited,returned,declined,resubmitted" {
		t.Fatalf("resubmitted = %s %s", resubmitted.State, resubmitted.kinds())
	}
	// The resubmission is measured against the approver's edit it declined.
	changes := resubmitted.History[4].Changes
	if before, after := variableChange(t, changes, "Subject"); before != `"Başvurular açık"` || after != `"GECEKODU başvuruları açıldı"` {
		t.Errorf("Subject %s → %s", before, after)
	}
	variableChange(t, changes, "Heading")
	if !resubmitted.SubmittedAt.Equal(w.now()) || !resubmitted.DeadlineAt.Equal(w.now().Add(7*24*time.Hour)) {
		t.Errorf("resubmitted %s, deadline %s: want a new seven days", resubmitted.SubmittedAt, resubmitted.DeadlineAt)
	}
	if n := len(w.mail.of(w.requested.ID)); n != 4 {
		t.Errorf("approval-requested mails = %d, want two approvers told twice", n)
	}

	if status, approved, failure := w.act("yusuf", submitted.ID, "approve", nil); status != fiber.StatusOK || approved.State != "approved" {
		t.Fatalf("approve the resubmission = %d %+v", status, failure)
	}
	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || !sameJSON(t, w.task(sent[0].taskID).BodyVariables, resend["body_variables"]) {
		t.Fatalf("sent %+v, want the resubmission", sent)
	}
}

// A rejection carries a reason, sends nothing, and tells the submitter why;
// the submitter may fix the request and resubmit it, and the rejection stays
// in its history.
func TestARejectionCarriesItsReasonAndCanBeResubmitted(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())

	for _, body := range []any{nil, map[string]any{}, map[string]any{"reason": "   "}} {
		if status, _, failure := w.act("fatih", submitted.ID, "reject", body); status != fiber.StatusBadRequest || failure.Code != "validation.error" {
			t.Errorf("reject with %v = %d %+v", body, status, failure)
		}
	}
	status, rejected, failure := w.act("fatih", submitted.ID, "reject", map[string]any{"reason": "Tarih yanlış: başvurular 6 Nisan'da kapanıyor."})
	if status != fiber.StatusOK || rejected.State != "rejected" || rejected.kinds() != "submitted,rejected" ||
		*rejected.History[1].Note != "Tarih yanlış: başvurular 6 Nisan'da kapanıyor." {
		t.Fatalf("reject = %d %+v %+v", status, failure, rejected.History)
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 0 {
		t.Fatalf("a rejection sent %d mails", n)
	}
	notices := w.mail.of(w.resolved.ID)
	if len(notices) != 1 || notices[0].variables["Decision"] != "rejected" ||
		notices[0].variables["DecisionNote"] != "Tarih yanlış: başvurular 6 Nisan'da kapanıyor." || notices[0].variables["DecidedBy"] != "Fatih Naz" {
		t.Fatalf("approval-resolved = %+v", notices)
	}
	if status, _, failure := w.act("yusuf", submitted.ID, "approve", nil); status != fiber.StatusConflict || failure.Params["state"] != "rejected" {
		t.Errorf("approving a rejected request = %d %+v", status, failure)
	}
	// Only the submitter resubmits, and only what was rejected or declined.
	if status, _, _ := w.act("fatih", submitted.ID, "resubmit", w.listSend()); status != fiber.StatusForbidden {
		t.Errorf("an approver resubmitting = %d", status)
	}
	pending := w.submit("elif", w.listSend())
	if status, _, failure := w.act("elif", pending.ID, "resubmit", w.listSend()); status != fiber.StatusConflict || failure.Params["state"] != "pending" {
		t.Errorf("resubmitting a pending request = %d %+v", status, failure)
	}

	fixed := w.listSend()
	fixed["body_variables"].(map[string]any)["BodyHtml"] = "<p>Başvurular <strong>6 Nisan</strong>'da kapanıyor.</p>"
	fixed["mail_list_id"] = w.group
	status, resubmitted, failure := w.act("elif", submitted.ID, "resubmit", fixed)
	if status != fiber.StatusOK || resubmitted.State != "pending" || resubmitted.kinds() != "submitted,rejected,resubmitted" {
		t.Fatalf("resubmit = %d %+v %s", status, failure, resubmitted.kinds())
	}
	changes := resubmitted.History[2].Changes
	if len(changes) != 2 || changes[0].Field != "audience" || !strings.Contains(string(changes[0].After), w.group.String()) {
		t.Errorf("resubmitted changes = %+v", changes)
	}
	variableChange(t, changes, "BodyHtml")
	if resubmitted.History[1].Kind != "rejected" || *resubmitted.History[1].Note == "" {
		t.Errorf("the rejection left the history: %+v", resubmitted.History)
	}

	// A resubmission is checked as a submission is.
	if status, _, failure := w.act("elif", pending.ID, "reject", map[string]any{"reason": "x"}); status != fiber.StatusForbidden || failure.Code != "server.forbidden" {
		t.Errorf("the submitter rejecting = %d %+v", status, failure)
	}
	w.act("fatih", pending.ID, "reject", map[string]any{"reason": "Liste yanlış."})
	broken := w.listSend()
	delete(broken["body_variables"].(map[string]any), "Subject")
	if status, _, failure := w.act("elif", pending.ID, "resubmit", broken); status != fiber.StatusUnprocessableEntity || failure.Code != "mail_approval.required_variables_missing" {
		t.Errorf("resubmitting without a subject = %d %+v", status, failure)
	}

	if status, approved, failure := w.act("yusuf", submitted.ID, "approve", nil); status != fiber.StatusOK || approved.State != "approved" {
		t.Fatalf("approve the resubmission = %d %+v", status, failure)
	}
	sent := w.mail.of(w.freeBasic.ID)
	if len(sent) != 1 || sent[0].kind != "group" || !sameJSON(t, w.task(sent[0].taskID).BodyVariables, fixed["body_variables"]) {
		t.Fatalf("sent %+v, want the resubmission to the group", sent)
	}
}

// A request undecided seven days after it was submitted expires: it can no
// longer be approved or accepted, it is never sent, and its submitter hears
// so once. Acting on it, reading it or listing it expires it on the spot;
// the sweep expires the ones no one looks at.
func TestAnUndecidedRequestExpiresAndIsNeverSent(t *testing.T) {
	w := newApprovalWorld(t)
	pending := w.submit("elif", w.listSend())
	returned := w.submit("elif", w.listSend())
	edit := w.listSend()["body_variables"].(map[string]any)
	edit["Heading"] = "Son gün"
	w.act("fatih", returned.ID, "return", map[string]any{"body_variables": edit})
	rejected := w.submit("elif", w.listSend())
	w.act("fatih", rejected.ID, "reject", map[string]any{"reason": "Hayır."})

	w.advance(7*24*time.Hour - time.Second)
	if _, read := w.get("fatih", pending.ID); read.State != "pending" {
		t.Fatalf("a second before its deadline: %s", read.State)
	}
	w.advance(time.Second)

	status, _, failure := w.act("fatih", pending.ID, "approve", nil)
	if status != fiber.StatusConflict || failure.Code != "mail_approval.expired" {
		t.Fatalf("approving at the deadline = %d %+v", status, failure)
	}
	status, _, failure = w.act("elif", returned.ID, "accept", nil)
	if status != fiber.StatusConflict || failure.Code != "mail_approval.expired" {
		t.Fatalf("accepting at the deadline = %d %+v", status, failure)
	}
	for _, id := range []uuid.UUID{pending.ID, returned.ID} {
		_, read := w.get("elif", id)
		if read.State != "expired" || read.History[len(read.History)-1].Kind != "expired" || read.History[len(read.History)-1].Actor != nil {
			t.Errorf("after the deadline: %s %s", read.State, read.kinds())
		}
		if status, _, failure := w.act("fatih", id, "approve", nil); status != fiber.StatusConflict || failure.Code != "mail_approval.state_conflict" {
			t.Errorf("approving an expired request = %d %+v", status, failure)
		}
	}
	// A rejected request is not waiting on anyone, so it does not expire.
	if _, read := w.get("elif", rejected.ID); read.State != "rejected" {
		t.Errorf("a rejected request past its deadline is %s", read.State)
	}
	if n := len(w.mail.of(w.freeBasic.ID)); n != 0 {
		t.Fatalf("expired requests sent %d mails", n)
	}
	var expiredNotices []sentMail
	for _, notice := range w.mail.of(w.resolved.ID) {
		if notice.variables["Decision"] == "expired" {
			expiredNotices = append(expiredNotices, notice)
		}
	}
	if len(expiredNotices) != 2 || expiredNotices[0].recipients[0] != elif.email || expiredNotices[0].sentBy != "skymail" ||
		expiredNotices[0].variables["DecidedBy"] != "SkyMail" || expiredNotices[0].variables["DecisionNote"] == "" {
		t.Fatalf("expiry notices = %+v", expiredNotices)
	}

	// The sweep expires what no one opened, each once.
	unread := w.submit("elif", w.listSend())
	unreadToo := w.submit("elif", w.listSend())
	w.advance(8 * 24 * time.Hour)
	expired, err := w.handler.ExpireDue(context.Background())
	if err != nil || expired != 2 {
		t.Fatalf("sweep = %d, %v; want the two unread requests", expired, err)
	}
	if expired, err := w.handler.ExpireDue(context.Background()); err != nil || expired != 0 {
		t.Fatalf("second sweep = %d, %v; want nothing", expired, err)
	}
	for _, id := range []uuid.UUID{unread.ID, unreadToo.ID} {
		if _, read := w.get("elif", id); read.State != "expired" {
			t.Errorf("after the sweep: %s", read.State)
		}
	}
	if n := len(w.mail.of(w.resolved.ID)); n != 2+2+2 {
		t.Errorf("approval-resolved mails = %d, want a return, a rejection and four expiries", n)
	}

	// Listing expires what is due before it lists.
	listed := w.submit("elif", w.listSend())
	w.advance(8 * 24 * time.Hour)
	var items []approvalAnswer
	w.call("fatih", fiber.MethodGet, "/v1/mail_approvals?state=pending", nil, &items)
	if len(items) != 0 {
		t.Errorf("pending after the deadline: %d", len(items))
	}
	if _, read := w.get("elif", listed.ID); read.State != "expired" {
		t.Errorf("listed request is %s", read.State)
	}
}

// Two approvers approving at once send the mail once: the request is locked
// while it is sent, and whoever comes second is told someone else is on it.
func TestTwoApproversAtOnceSendOnce(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())
	w.mail.gate = make(chan struct{})
	w.mail.entered = make(chan struct{}, 2)

	type result struct {
		status int
		code   string
	}
	results := make(chan result, 2)
	for _, who := range []string{"fatih", "yusuf"} {
		go func(who string) {
			status, _, failure := w.act(who, submitted.ID, "approve", nil)
			results <- result{status, failure.Code}
		}(who)
	}

	// One approval is inside the mailer with the request locked. The other
	// finishes while it is, refused.
	select {
	case <-w.mail.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no approval reached the mailer")
	}
	first := <-results
	if first.status != fiber.StatusConflict || first.code != "mail_approval.busy" {
		t.Fatalf("the approval that came second = %+v, want 409 mail_approval.busy", first)
	}
	close(w.mail.gate)
	if second := <-results; second.status != fiber.StatusOK {
		t.Fatalf("the approval that came first = %+v", second)
	}

	if sent := w.mail.of(w.freeBasic.ID); len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	_, read := w.get("elif", submitted.ID)
	if read.State != "approved" || read.kinds() != "submitted,approved" {
		t.Fatalf("after two approvals: %s %s", read.State, read.kinds())
	}
	var tasks int64
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks WHERE template_id = $1`, w.freeBasic.ID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 {
		t.Fatalf("mail_tasks of the template = %d, want 1", tasks)
	}
}

// Deciding takes the approver's role, and an approver does not decide their
// own request. A submitter sees their own requests; an approver sees all; a
// member who is neither sees none but their own.
func TestWhoMaySeeAndDecideARequest(t *testing.T) {
	w := newApprovalWorld(t)
	byElif := w.submit("elif", w.listSend())
	w.advance(time.Second)
	byYusuf := w.submit("yusuf", w.listSend())

	for _, action := range []string{"approve", "return", "reject"} {
		body := map[string]any{"reason": "x", "body_variables": map[string]any{"Subject": "y", "BodyHtml": "z"}}
		for _, who := range []string{"elif", "baska"} {
			status, _, failure := w.act(who, byElif.ID, action, body)
			if status != fiber.StatusForbidden || failure.Code != "server.forbidden" {
				t.Errorf("%s %s = %d %+v, want 403", who, action, status, failure)
			}
		}
		status, _, failure := w.act("yusuf", byYusuf.ID, action, body)
		if status != fiber.StatusForbidden || failure.Code != "mail_approval.own_request" {
			t.Errorf("yusuf %s on his own = %d %+v", action, status, failure)
		}
	}
	for _, action := range []string{"accept", "decline", "resubmit"} {
		if status, _, _ := w.act("baska", byElif.ID, action, w.listSend()); status != fiber.StatusNotFound {
			t.Errorf("baska %s = %d, want 404", action, status)
		}
	}

	if status, _ := w.get("baska", byElif.ID); status != fiber.StatusNotFound {
		t.Errorf("baska reading elif's = %d, want 404", status)
	}
	if status, _ := w.get("elif", byYusuf.ID); status != fiber.StatusNotFound {
		t.Errorf("elif reading yusuf's = %d, want 404", status)
	}
	if status, read := w.get("fatih", byElif.ID); status != fiber.StatusOK || read.Preview == nil || read.RecipientCount == nil || *read.RecipientCount != 2 {
		t.Errorf("an approver reading = %d %+v", status, read)
	}
	if status, _ := w.get("elif", uuid.New()); status != fiber.StatusNotFound {
		t.Errorf("an unknown request = %d", status)
	}

	list := func(who, query string) ([]uuid.UUID, string) {
		t.Helper()
		var items []approvalAnswer
		request := httptest.NewRequest(fiber.MethodGet, "/v1/mail_approvals"+query, nil)
		request.Header.Set("X-Test-User", who)
		response, err := w.app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
			t.Fatalf("GET %s as %s = %d", query, who, response.StatusCode)
		}
		var ids []uuid.UUID
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		return ids, response.Header.Get("X-Total-Count")
	}
	w.advance(time.Minute)
	later := w.submit("elif", w.listSend())
	w.act("fatih", byElif.ID, "reject", map[string]any{"reason": "Tekrar bak."})

	for _, tc := range []struct {
		who, query string
		want       []uuid.UUID
	}{
		{"fatih", "", []uuid.UUID{later.ID, byYusuf.ID, byElif.ID}},
		{"fatih", "?state=pending", []uuid.UUID{later.ID, byYusuf.ID}},
		{"fatih", "?mine=true", nil},
		{"yusuf", "?mine=true", []uuid.UUID{byYusuf.ID}},
		{"elif", "", []uuid.UUID{later.ID, byElif.ID}},
		{"elif", "?state=rejected", []uuid.UUID{byElif.ID}},
		{"baska", "", nil},
		{"fatih", "?_start=1&_end=2", []uuid.UUID{byYusuf.ID}},
	} {
		got, total := list(tc.who, tc.query)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s lists %q: %v, want %v", tc.who, tc.query, got, tc.want)
		}
		if tc.query == "?_start=1&_end=2" && total != "3" {
			t.Errorf("X-Total-Count = %s, want 3", total)
		}
	}
	var items []approvalAnswer
	w.call("elif", fiber.MethodGet, "/v1/mail_approvals?state=rejected", nil, &items)
	if len(items) != 1 || items[0].LastEvent == nil || items[0].LastEvent.Kind != "rejected" || *items[0].LastEvent.Note != "Tekrar bak." {
		t.Errorf("a rejected request's last event in the list = %+v", items)
	}
	if status, raw := w.call("fatih", fiber.MethodGet, "/v1/mail_approvals?state=bogus", nil, nil); status != fiber.StatusBadRequest {
		t.Errorf("an unknown state = %d %s", status, raw)
	}
}

// A request is pinned to the version its template published when it was
// submitted. Once the template publishes another, sending the request would
// not send what was submitted, so approving it is refused until the submitter
// resubmits on the new version.
func TestARequestIsNotSentOverARepublishedTemplate(t *testing.T) {
	w := newApprovalWorld(t)
	submitted := w.submit("elif", w.listSend())

	republished := w.seedTemplate("free.basic", "Serbest Gönderim", "{{.Subject}}",
		`<h1>{{.Heading}}</h1><div>{{safeHTML .BodyHtml}}</div><footer>SKY LAB</footer>`, "{{.BodyHtml}}",
		`[{"name":"BodyHtml","reason":null},{"name":"Subject","reason":"Konu gönderimde yazılır."}]`)
	if *republished.PublishedVersionID == submitted.Template.VersionID {
		t.Fatal("the reseed published no new version")
	}

	_, read := w.get("fatih", submitted.ID)
	if !read.Template.Republished || read.Preview == nil || strings.Contains(read.Preview.HTML, "<footer>") {
		t.Errorf("read after the republish: republished %v, preview %+v", read.Template.Republished, read.Preview)
	}
	status, _, failure := w.act("fatih", submitted.ID, "approve", nil)
	if status != fiber.StatusConflict || failure.Code != "mail_approval.template_republished" ||
		failure.Params["published_version_id"] != republished.PublishedVersionID.String() {
		t.Fatalf("approve over a republished template = %d %+v", status, failure)
	}
	if _, read := w.get("fatih", submitted.ID); read.State != "pending" || len(w.mail.of(w.freeBasic.ID)) != 0 {
		t.Fatalf("a refused approval left %s and %d sends", read.State, len(w.mail.of(w.freeBasic.ID)))
	}

	w.act("fatih", submitted.ID, "reject", map[string]any{"reason": "Şablon değişti, yeniden sun."})
	_, resubmitted, _ := w.act("elif", submitted.ID, "resubmit", w.listSend())
	if resubmitted.Template.VersionID != *republished.PublishedVersionID || resubmitted.Template.Republished ||
		resubmitted.History[2].Changes[0].Field != "template" {
		t.Fatalf("resubmitted on %s, changes %+v", resubmitted.Template.VersionID, resubmitted.History[2].Changes)
	}
	if status, _, failure := w.act("fatih", submitted.ID, "approve", nil); status != fiber.StatusOK {
		t.Fatalf("approve after resubmitting = %d %+v", status, failure)
	}
	rows := w.queuedRows(w.mail.of(w.freeBasic.ID)[0].taskID)
	if !strings.Contains(*rows["ayse@example.com"].BodyHtml, "<footer>SKY LAB</footer>") {
		t.Errorf("sent %q, want the new version", *rows["ayse@example.com"].BodyHtml)
	}
}

// A notification that cannot go out does not undo what it was about, and the
// answer says why it went nowhere.
func TestANotificationThatCannotGoOutSaysWhy(t *testing.T) {
	w := newApprovalWorld(t)

	noAddress := w.submit("adsiz", w.listSend())
	status, rejected, failure := w.act("fatih", noAddress.ID, "reject", map[string]any{"reason": "Eksik."})
	if status != fiber.StatusOK || rejected.State != "rejected" || rejected.Notification == nil ||
		rejected.Notification.Problem == nil || *rejected.Notification.Problem != "no_address" || rejected.Notification.Notified != 0 {
		t.Fatalf("rejecting for a submitter with no address = %d %+v %+v", status, failure, rejected.Notification)
	}

	if _, err := w.store.ArchiveTemplate(context.Background(), database.ArchiveTemplateParams{ID: w.requested.ID}); err != nil {
		t.Fatal(err)
	}
	unseeded := w.submit("elif", w.listSend())
	if unseeded.State != "pending" || unseeded.Notification == nil || unseeded.Notification.Problem == nil ||
		*unseeded.Notification.Problem != "template_unavailable" {
		t.Fatalf("submitting with no approval-requested template = %+v", unseeded.Notification)
	}
}

// The screens are built from the OpenAPI document, so every approval route
// is in it with its answer.
func TestOpenAPIDocumentDescribesMailApproval(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, nil)

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/docs/openapi.json", nil))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Tags      []string                   `json:"tags"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	for path, methods := range map[string][]string{
		"/mail_approvals":               {"get", "post"},
		"/mail_approvals/{id}":          {"get"},
		"/mail_approvals/{id}/approve":  {"post"},
		"/mail_approvals/{id}/return":   {"post"},
		"/mail_approvals/{id}/reject":   {"post"},
		"/mail_approvals/{id}/accept":   {"post"},
		"/mail_approvals/{id}/decline":  {"post"},
		"/mail_approvals/{id}/resubmit": {"post"},
	} {
		for _, method := range methods {
			operation, ok := document.Paths[path][method]
			if !ok || len(operation.Tags) != 1 || operation.Tags[0] != "Mail approval" || operation.Responses["409"] == nil && method == "post" && path != "/mail_approvals" {
				t.Errorf("%s %s documented as %+v", method, path, operation)
			}
		}
	}
}
