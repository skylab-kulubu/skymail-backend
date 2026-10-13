package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// renderedSend is a single mail whose queue row holds what the template
// rendered for its recipient, as a send stores it.
type renderedSend struct {
	key                      string
	sensitive                *bool
	subject, plainText, html string
	variables                map[string]any
	sentBy                   string
	createdAt                time.Time
	cleared                  bool
}

func seedRenderedSend(t *testing.T, db *database.Store, send renderedSend) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	variables, err := json.Marshal(send.variables)
	if err != nil {
		t.Fatal(err)
	}
	recipient := mailer.RecipientInfo{FullName: "Ayşe Kaya", Email: "ayse@example.com"}
	rendered, err := mailer.Render(send.subject, send.plainText, send.html, variables, recipient)
	if err != nil {
		t.Fatal(err)
	}
	var key *string
	if send.key != "" {
		key = &send.key
	}
	var templateID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO templates (name, key, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		VALUES ('Şablon', $1, $2, $3, $4, '', $5) RETURNING id`,
		key, send.subject, send.html, send.plainText, send.sensitive).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	var cleared *time.Time
	if send.cleared {
		cleared = &send.createdAt
		rendered = mailer.Rendered{}
		variables = []byte(`{}`)
	}
	var taskID, rowID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, created_at, content_cleared_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		send.sentBy, templateID, variables, send.createdAt, cleared).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	var html *string
	if rendered.HTML != "" {
		html = &rendered.HTML
	}
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'sent', $7) RETURNING id`,
		taskID, recipient.FullName, recipient.Email, rendered.Subject, rendered.PlainText, html, send.createdAt).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	return taskID, rowID
}

// assertNoSecret fails for a secret, or any part of a link's query, found in
// the answer as JSON — the subject, the plain text, the HTML and its hrefs,
// escaped as html/template and JSON escape them or not.
func assertNoSecret(t *testing.T, name string, answer contentAnswer, secrets ...string) {
	t.Helper()
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	unescaped := strings.NewReplacer(`&`, "&", `<`, "<", `>`, ">", "&amp;", "&", `\/`, "/").Replace(string(body))
	for _, secret := range secrets {
		pieces := []string{secret}
		if i := strings.Index(secret, "?"); i >= 0 {
			for _, pair := range strings.Split(secret[i+1:], "&") {
				if _, value, ok := strings.Cut(pair, "="); ok && len(value) >= 6 {
					pieces = append(pieces, value)
				}
			}
		}
		for _, piece := range pieces {
			if strings.Contains(strings.ToLower(unescaped), strings.ToLower(piece)) {
				t.Errorf("%s: the answer holds %q: %s", name, piece, body)
			}
		}
	}
}

const (
	resetLink = "https://e.yildizskylab.com/realms/e-skylab/login-actions/action-token?key=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJheXNlIn0.c2ln&client_id=account"
	loginCode = "604918"
)

// keycloakLike is a Keycloak mail as the seed writes it: the link in an href
// and in the text, a code, the person's name.
func keycloakLike(key string, sentBy string, createdAt time.Time) renderedSend {
	return renderedSend{
		key:       key,
		subject:   "Parola sıfırlama — kodun {{.code}}",
		plainText: "Merhaba {{.firstName}},\nBağlantı: {{.link}}\nKod: {{.code}}\n{{.linkExpirationMinutes}} dakika geçerli.",
		html: `<p>Merhaba {{.firstName}},</p><p><a href="{{.link}}">Parolamı sıfırla</a></p>` +
			`<p>Açılmazsa: {{.link}}</p><p>Kod: <b>{{.code}}</b></p><p>{{.linkExpirationMinutes}} dakika geçerli.</p>`,
		variables: map[string]any{
			"link": resetLink, "linkExpirationMinutes": "60", "code": loginCode, "codeExpirationMinutes": "10",
			"firstName": "Ayşe", "username": "ayse", "realmDisplayName": "SKY LAB", "subjectKey": "passwordResetSubject",
		},
		sentBy:    sentBy,
		createdAt: createdAt,
	}
}

// A sensitive mail is shown whole, its code and its link masked on the server:
// the unmasked value is in no part of the answer. The opening is recorded.
func TestSensitiveContentIsShownMaskedAndTheOpeningRecorded(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", "service-account-keycloak-mailer", now.Add(-time.Hour)))

	status, body := getAs(t, app, "/mail_tasks/"+task.String()+"/queue/"+row.String()+"/content",
		contentOther, "skymail:access", "skymail:mails:read", SystemMailsReadRole)
	if status != 200 {
		t.Fatalf("content = %d %s", status, body)
	}
	var answer contentAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Access != "masked" || answer.Subject == nil || answer.PlainText == nil || answer.HTML == nil {
		t.Fatalf("content = %s, want masked content", body)
	}
	assertNoSecret(t, "keycloak mail", answer, resetLink, loginCode, "action-token")
	for name, part := range map[string]string{"subject": *answer.Subject, "plain text": *answer.PlainText, "html": *answer.HTML} {
		if !strings.Contains(part, mailer.Mask) {
			t.Errorf("%s does not show the mask: %q", name, part)
		}
	}
	for _, open := range []string{"Merhaba Ayşe", "60 dakika geçerli", "Parolamı sıfırla"} {
		if !strings.Contains(*answer.HTML, open) {
			t.Errorf("html lost %q: %s", open, *answer.HTML)
		}
	}
	if !strings.Contains(*answer.HTML, `<mark data-skymail-masked="">`) {
		t.Errorf("html does not mark what it masked: %s", *answer.HTML)
	}
	if answer.MaskedBy == nil || *answer.MaskedBy != "variables" || strings.Join(answer.MaskedVariables, ",") != "code,link" {
		t.Errorf("masked_by %v, masked_variables %v; want variables, [code link]", answer.MaskedBy, answer.MaskedVariables)
	}
	if answer.ExpiresAt == nil || !answer.ExpiresAt.Equal(now.Add(-time.Hour).Add(90*24*time.Hour)) {
		t.Errorf("expires_at = %v", answer.ExpiresAt)
	}

	var views int
	var actorSub, actorName, maskedBy string
	var maskedVariables []string
	var queueItem uuid.UUID
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT count(*) OVER (), actor_sub, COALESCE(actor_name, ''), masked_by, masked_variables, queue_item_id
		FROM sent_content_views WHERE task_id = $1`, task).Scan(&views, &actorSub, &actorName, &maskedBy, &maskedVariables, &queueItem); err != nil {
		t.Fatalf("the opening was not recorded: %v", err)
	}
	if views != 1 || actorSub != contentOther || maskedBy != "variables" || strings.Join(maskedVariables, ",") != "code,link" || queueItem != row {
		t.Errorf("recorded %d: %s %q %s %v %s", views, actorSub, actorName, maskedBy, maskedVariables, queueItem)
	}

	// A second opening is a second row, with the name the token carried.
	request := "/mail_tasks/" + task.String() + "/queue/" + row.String() + "/content"
	if status, body := getAsNamed(t, app, request, contentSender, "Yusuf Açmacı", "skymail:access", "skymail:mails:read", SystemMailsReadRole); status != 200 {
		t.Fatalf("second opening = %d %s", status, body)
	}
	if n := countViews(t, db, task); n != 2 {
		t.Errorf("openings recorded = %d, want 2", n)
	}
	var name string
	if err := db.Conn.QueryRow(context.Background(), `SELECT actor_name FROM sent_content_views WHERE actor_sub = $1`, contentSender).Scan(&name); err != nil || name != "Yusuf Açmacı" {
		t.Errorf("actor_name = %q (%v), want the token's name", name, err)
	}

	// Who may not read it learns nothing, and nothing is recorded.
	if answer := contentAs(t, app, task, row, contentOther, "skymail:access", "skymail:mails:read", "skymail:mails:write"); answer.Access != "restricted" || answer.Subject != nil {
		t.Errorf("a list sender: %+v, want restricted", answer)
	}
	if n := countViews(t, db, task); n != 2 {
		t.Errorf("openings recorded = %d after a restricted read, want 2", n)
	}
}

func getAsNamed(t *testing.T, app *fiber.App, path, sub, name string, roles ...string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, path, nil)
	request.Header.Set("X-Test-Sub", sub)
	request.Header.Set("X-Test-Name", name)
	request.Header.Set("X-Test-Roles", strings.Join(roles, ","))
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func countViews(t *testing.T, db *database.Store, task uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.Conn.QueryRow(context.Background(), `SELECT count(*) FROM sent_content_views WHERE task_id = $1`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A mail that may be shown is shown as sent, link and all, and its opening
// is not recorded.
func TestShownContentIsAsSentAndNotRecorded(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	no := false
	send := keycloakLike("core.welcome", contentSender, now.Add(-time.Hour))
	send.sensitive = &no
	task, row := seedRenderedSend(t, db, send)

	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "available" || answer.HTML == nil || !strings.Contains(*answer.HTML, "action-token?key=") ||
		!strings.Contains(*answer.PlainText, resetLink) || !strings.Contains(*answer.Subject, loginCode) {
		t.Errorf("content = %+v, want the mail as sent", answer)
	}
	if answer.MaskedBy != nil || answer.MaskedVariables != nil {
		t.Errorf("masked_by %v, masked_variables %v; want null", answer.MaskedBy, answer.MaskedVariables)
	}
	if n := countViews(t, db, task); n != 0 {
		t.Errorf("openings recorded = %d, want 0", n)
	}
}

// The template changed since the send: the version the send rendered masks
// it, so the masked mail is the mail that was sent.
func TestSensitiveContentIsMaskedWithTheVersionTheSendUsed(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	send := keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour))
	task, row := seedRenderedSend(t, db, send)
	ctx := context.Background()
	if _, err := db.Conn.Exec(ctx, `
		WITH tmpl AS (SELECT template_id AS id FROM mail_tasks WHERE id = $1)
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, created_at, published_at)
		SELECT tmpl.id, 1, 'Şablon', $2, $3, 'html', $3, $4, 'template_seed', $5, $5 FROM tmpl`,
		task, send.subject, send.html, send.plainText, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(ctx, `
		UPDATE templates SET html_content = '<p>Yeni tasarım {{.firstName}}</p><a href="{{.link}}">Sıfırla</a>'
		WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task); err != nil {
		t.Fatal(err)
	}

	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "masked" || answer.MaskedBy == nil || *answer.MaskedBy != "variables" {
		t.Fatalf("content = %+v, want masked by variables", answer)
	}
	if !strings.Contains(*answer.HTML, "Parolamı sıfırla") || strings.Contains(*answer.HTML, "Yeni tasarım") {
		t.Errorf("html = %s, want the version the send used", *answer.HTML)
	}
	assertNoSecret(t, "older version", answer, resetLink, loginCode)
}

// Account erasure emptied the send's variables and the template changed: the
// stored mail is masked by patterns, and no link or code is left.
func TestSensitiveContentIsMaskedByPatternsWhenNothingRendersIt(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour)))
	if _, err := db.Conn.Exec(context.Background(), `
		UPDATE mail_tasks SET body_variables = '{}' WHERE id = $1;`, task); err != nil {
		t.Fatal(err)
	}

	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "masked" || answer.MaskedBy == nil || *answer.MaskedBy != "patterns" {
		t.Fatalf("content = %+v, want masked by patterns", answer)
	}
	assertNoSecret(t, "patterns", answer, resetLink, loginCode, "action-token")
	if !strings.Contains(*answer.HTML, "Merhaba Ayşe") {
		t.Errorf("html = %s, want the rest open", *answer.HTML)
	}
}

// A credential the masking cannot hide for certain — here the token again,
// in another case, in a variable that is not a link — is not shown at all.
func TestSensitiveContentThatCannotBeMaskedIsNotShown(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, renderedSend{
		key:       "keycloak.generic",
		subject:   "Bağlantın",
		plainText: "{{.link}} {{.Note}}",
		html:      `<a href="{{.link}}">aç</a> {{.Note}}`,
		variables: map[string]any{"link": "https://x.example/y?key=SecretValue42", "Note": "referans secretvalue42"},
		sentBy:    contentSender,
		createdAt: now.Add(-time.Hour),
	})
	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "sensitive" || answer.Subject != nil || answer.PlainText != nil || answer.HTML != nil {
		t.Errorf("content = %+v, want sensitive with nothing in it", answer)
	}
	if n := countViews(t, db, task); n != 0 {
		t.Errorf("openings recorded = %d, want 0", n)
	}
}

// A sensitive send the sweep emptied within the retention period — before
// sensitive content was kept — says so, not that it is secret or expired.
func TestSensitiveSendEmptiedBeforeItWasKeptIsNotKept(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 13, 15, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	system := []string{"skymail:access", "skymail:mails:read", SystemMailsReadRole}

	sensitive := keycloakLike("keycloak.reset-password", contentSender, now.Add(-48*time.Hour))
	sensitive.cleared = true
	task, row := seedRenderedSend(t, db, sensitive)
	if answer := contentAs(t, app, task, row, contentOther, system...); answer.Access != "not_kept" || answer.Subject != nil || answer.HTML != nil {
		t.Errorf("emptied sensitive send: %+v, want not_kept", answer)
	}
	status, body := getAs(t, app, "/mail_tasks/"+task.String(), contentOther, system...)
	var send struct {
		ContentAccess string `json:"content_access"`
	}
	if err := json.Unmarshal(body, &send); status != 200 || err != nil || send.ContentAccess != "not_kept" {
		t.Errorf("send = %d %s, want content_access not_kept", status, body)
	}
	// Who may not read it learns only that.
	if answer := contentAs(t, app, task, row, contentOther, "skymail:access", "skymail:mails:read"); answer.Access != "restricted" {
		t.Errorf("a reader: %+v, want restricted", answer)
	}

	// One that may be shown, emptied within the period (a shorter retention
	// than now), is expired.
	no := false
	shown := keycloakLike("core.welcome", contentSender, now.Add(-48*time.Hour))
	shown.sensitive, shown.cleared = &no, true
	shownTask, shownRow := seedRenderedSend(t, db, shown)
	if answer := contentAs(t, app, shownTask, shownRow, contentOther, system...); answer.Access != "expired" {
		t.Errorf("emptied shown send: %+v, want expired", answer)
	}
	if n := countViews(t, db, task); n != 0 {
		t.Errorf("openings recorded = %d, want 0", n)
	}
}
