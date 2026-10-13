package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// maskNow runs the content sweep's masking, as every replica does every few
// minutes; a send that fails is tried again a millisecond later.
func maskNow(t *testing.T, db *database.Store) mailer.MaskResult {
	t.Helper()
	result, err := mailer.MaskSensitiveContent(context.Background(), db, mailer.MaskOptions{RowsPerChunk: 2, RetryAfter: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// renderedSend is a single mail whose queue row holds what the template
// rendered for its recipient, as a send stores it. The template was written
// a minute before the send.
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
	written := send.createdAt.Add(-time.Minute)
	var templateID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO templates (name, key, subject, html_content, plain_text_content, react_email_content, content_sensitive,
		                       created_at, updated_at)
		VALUES ('Şablon', $1, $2, $3, $4, '', $5, $6, $6) RETURNING id`,
		key, send.subject, send.html, send.plainText, send.sensitive, written).Scan(&templateID); err != nil {
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
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, created_at, content_cleared_at, content_sensitive_at_send)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::boolean::text, 'unset')) RETURNING id`,
		send.sentBy, templateID, variables, send.createdAt, cleared, send.sensitive).Scan(&taskID); err != nil {
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
// the text: an answer as JSON, or what the database keeps.
func assertNoSecret(t *testing.T, name string, answer any, secrets ...string) {
	t.Helper()
	body, ok := answer.(string)
	if !ok {
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		body = string(encoded)
	}
	unescaped := strings.NewReplacer(`&`, "&", `<`, "<", `>`, ">", "&amp;", "&", `\/`, "/").Replace(body)
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
				t.Errorf("%s: holds %q: %s", name, piece, body)
			}
		}
	}
}

// keptContent is everything the database keeps of a send's content: its
// rows' mails and its variables.
func keptContent(t *testing.T, db *database.Store, task uuid.UUID) string {
	t.Helper()
	var kept string
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT mt.body_variables::text || ' ' || COALESCE(string_agg(q.subject || ' ' || q.body || ' ' || COALESCE(q.body_html, ''), ' '), '')
		FROM mail_tasks mt LEFT JOIN mail_queue q ON q.task_id = mt.id
		WHERE mt.id = $1 GROUP BY mt.id`, task).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	return kept
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
			"link": resetLink, "linkExpirationMinutes": "1440", "code": loginCode, "codeExpirationMinutes": "10",
			"firstName": "Ayşe", "username": "ayse", "realmDisplayName": "SKY LAB", "subjectKey": "passwordResetSubject",
		},
		sentBy:    sentBy,
		createdAt: createdAt,
	}
}

func getAsNamed(t *testing.T, app *fiber.App, path, sub, name string, roles ...string) (int, []byte, string) {
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
	return response.StatusCode, body, response.Header.Get(fiber.HeaderCacheControl)
}

func countViews(t *testing.T, db *database.Store, task uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.Conn.QueryRow(context.Background(), `SELECT count(*) FROM sent_content_views WHERE task_id = $1`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A sensitive mail is masked when it is stored, once the send is done: the
// database keeps no code or link, and the answer holds none. Until then it is
// pending, not shown. Every opening of the masked mail is recorded.
func TestSensitiveContentIsMaskedWhenStoredAndTheOpeningRecorded(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	system := []string{"skymail:access", "skymail:mails:read", SystemMailsReadRole}
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", "service-account-keycloak-mailer", now.Add(-time.Hour)))
	path := "/mail_tasks/" + task.String() + "/queue/" + row.String() + "/content"

	// Not masked yet: not shown.
	if answer := contentAs(t, app, task, row, contentOther, system...); answer.Access != "pending" || answer.Subject != nil || answer.HTML != nil {
		t.Fatalf("before the sweep: %+v, want pending with nothing in it", answer)
	}

	if result := maskNow(t, db); result.Sends != 1 || result.Rows != 1 || result.NotKept != 0 {
		t.Fatalf("sweep = %+v, want one send, one row masked", result)
	}
	kept := keptContent(t, db, task)
	assertNoSecret(t, "what the database keeps", kept, resetLink, loginCode, "action-token")
	if !strings.Contains(kept, "1440") || !strings.Contains(kept, "Ayşe") {
		t.Errorf("the rest is not kept: %s", kept)
	}

	status, body, cache := getAsNamed(t, app, path, contentOther, "Yusuf Açmacı", system...)
	if status != 200 || cache != "no-store" {
		t.Fatalf("content = %d (Cache-Control %q) %s", status, cache, body)
	}
	var answer contentAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Access != "masked" || answer.Subject == nil || answer.PlainText == nil || answer.HTML == nil {
		t.Fatalf("content = %s, want masked content", body)
	}
	assertNoSecret(t, "the answer", answer, resetLink, loginCode, "action-token")
	for name, part := range map[string]string{"subject": *answer.Subject, "plain text": *answer.PlainText, "html": *answer.HTML} {
		if !strings.Contains(part, mailer.Mask) {
			t.Errorf("%s does not show the mask: %q", name, part)
		}
	}
	for _, open := range []string{"Merhaba Ayşe", "1440 dakika geçerli", "Parolamı sıfırla"} {
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

	var actorSub, actorName, maskedBy string
	var maskedVariables []string
	var queueItem uuid.UUID
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT actor_sub, actor_name, masked_by, masked_variables, queue_item_id
		FROM sent_content_views WHERE task_id = $1`, task).Scan(&actorSub, &actorName, &maskedBy, &maskedVariables, &queueItem); err != nil {
		t.Fatalf("the opening was not recorded: %v", err)
	}
	if actorSub != contentOther || actorName != "Yusuf Açmacı" || maskedBy != "variables" ||
		strings.Join(maskedVariables, ",") != "code,link" || queueItem != row {
		t.Errorf("recorded: %s %q %s %v %s", actorSub, actorName, maskedBy, maskedVariables, queueItem)
	}
	if answer := contentAs(t, app, task, row, contentOther, system...); answer.Access != "masked" {
		t.Fatalf("second opening: %+v", answer)
	}
	if n := countViews(t, db, task); n != 2 {
		t.Errorf("openings recorded = %d, want 2", n)
	}

	// Who may not read it learns nothing, and nothing is recorded.
	if answer := contentAs(t, app, task, row, contentOther, "skymail:access", "skymail:mails:read", "skymail:mails:write"); answer.Access != "restricted" || answer.Subject != nil {
		t.Errorf("a list sender: %+v, want restricted", answer)
	}
	if n := countViews(t, db, task); n != 2 {
		t.Errorf("openings recorded = %d after a restricted read, want 2", n)
	}

	// A second sweep has nothing to do.
	if result := maskNow(t, db); result.Sends != 0 {
		t.Errorf("second sweep = %+v, want nothing", result)
	}
}

// No content leaves without the record of who opened it.
func TestMaskedContentIsNotShownWhenTheOpeningCannotBeRecorded(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour)))
	maskNow(t, db)
	if _, err := db.Conn.Exec(context.Background(), `
		CREATE FUNCTION refuse_view() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no record'; END $$;
		CREATE TRIGGER refuse_view BEFORE INSERT ON sent_content_views FOR EACH ROW EXECUTE FUNCTION refuse_view();`); err != nil {
		t.Fatal(err)
	}
	status, body := getAs(t, app, "/mail_tasks/"+task.String()+"/queue/"+row.String()+"/content", contentSender, "skymail:access", "skymail:mails:read")
	if status == 200 || strings.Contains(string(body), "Merhaba") || strings.Contains(string(body), "Parola") {
		t.Errorf("content with no record = %d %s, want an error and no content", status, body)
	}
}

// A mail that may be shown is shown as sent, link and all; the sweep leaves
// it, and its opening is not recorded.
func TestShownContentIsAsSentAndNotRecorded(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	no := false
	send := keycloakLike("core.welcome", contentSender, now.Add(-time.Hour))
	send.sensitive = &no
	task, row := seedRenderedSend(t, db, send)
	if result := maskNow(t, db); result.Sends != 0 {
		t.Fatalf("sweep masked %+v of a shown send", result)
	}

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

// A send sensitive when it was queued stays so: its template made shown
// afterwards, before or after the sweep, does not show it as sent.
func TestASendSensitiveWhenQueuedStaysMasked(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	yes := true
	for i, flipBeforeSweep := range []bool{true, false} {
		send := keycloakLike("core.new-mail-"+strconv.Itoa(i), contentSender, now.Add(-time.Hour))
		send.sensitive = &yes
		task, row := seedRenderedSend(t, db, send)
		flip := func() {
			if _, err := db.Conn.Exec(context.Background(), `
				UPDATE templates SET content_sensitive = false WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task); err != nil {
				t.Fatal(err)
			}
		}
		if flipBeforeSweep {
			flip()
		}
		maskNow(t, db)
		if !flipBeforeSweep {
			flip()
		}
		answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
		if answer.Access != "masked" {
			t.Errorf("flipped before the sweep %v: %+v, want masked", flipBeforeSweep, answer)
		}
		assertNoSecret(t, "flipped", answer, resetLink, loginCode)
		assertNoSecret(t, "kept", keptContent(t, db, task), resetLink, loginCode)
	}
}

// The sweep renders again only with the template as it stood when the send
// was queued: a version published after it — which could branch on the
// hidden code — is never used.
func TestMaskingUsesOnlyTheTemplateInForceWhenTheSendWasQueued(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	ctx := context.Background()
	send := keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour))
	task, row := seedRenderedSend(t, db, send)
	// The version the send rendered, published before it; then the row is
	// written after the send with a template that branches on the code.
	oracle := `{{if eq (slice .code 0 1) "6"}}` + send.html + `{{else}}NOMATCH{{end}}`
	if _, err := db.Conn.Exec(ctx, `
		WITH tmpl AS (SELECT template_id AS id FROM mail_tasks WHERE id = $1)
		INSERT INTO template_versions (template_id, seq, name, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, created_at, published_at)
		SELECT tmpl.id, v.seq, 'Şablon', $2, v.html, 'html', v.html, $3, 'template_seed', v.at, v.at
		FROM tmpl, (VALUES (1, $4::text, $5::timestamptz), (2, $6::text, $7::timestamptz)) AS v(seq, html, at)`,
		task, send.subject, send.plainText, send.html, now.Add(-2*time.Hour), oracle, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(ctx, `
		UPDATE templates SET html_content = $2, updated_at = now()
		WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task, oracle); err != nil {
		t.Fatal(err)
	}

	maskNow(t, db)
	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "masked" || answer.MaskedBy == nil || *answer.MaskedBy != "variables" {
		t.Fatalf("content = %+v, want masked by variables, with the version in force", answer)
	}
	if !strings.Contains(*answer.HTML, "Parolamı sıfırla") || strings.Contains(*answer.HTML, "NOMATCH") {
		t.Errorf("html = %s, want the version the send used", *answer.HTML)
	}
	assertNoSecret(t, "in force", answer, resetLink, loginCode)
}

// Account erasure emptied the send's variables and the template changed: the
// stored mail is masked by patterns, and no link or code is left.
func TestSensitiveContentIsMaskedByPatternsWhenNothingRendersIt(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour)))
	if _, err := db.Conn.Exec(context.Background(), `UPDATE mail_tasks SET body_variables = '{}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	maskNow(t, db)
	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "masked" || answer.MaskedBy == nil || *answer.MaskedBy != "patterns" {
		t.Fatalf("content = %+v, want masked by patterns", answer)
	}
	assertNoSecret(t, "patterns", answer, resetLink, loginCode, "action-token")
	assertNoSecret(t, "kept", keptContent(t, db, task), resetLink, loginCode, "action-token")
	if !strings.Contains(*answer.HTML, "Merhaba Ayşe") {
		t.Errorf("html = %s, want the rest open", *answer.HTML)
	}
}

// A credential the masking cannot hide for certain — here the link's key
// again, in another case and in no shape of a code — is not kept: the send
// is emptied and reads not kept.
func TestSensitiveContentThatCannotBeMaskedIsNotKept(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	task, row := seedRenderedSend(t, db, renderedSend{
		key:       "keycloak.generic",
		subject:   "Bağlantın",
		plainText: "{{.link}} {{.Note}}",
		html:      `<a href="{{.link}}">aç</a> {{.Note}}`,
		variables: map[string]any{"link": "https://x.example/y?key=abcdefgh", "Note": "referans ABCDEFGH"},
		sentBy:    contentSender,
		createdAt: now.Add(-time.Hour),
	})
	if result := maskNow(t, db); result.NotKept != 1 {
		t.Fatalf("sweep = %+v, want one send not kept", result)
	}
	assertNoSecret(t, "kept", keptContent(t, db, task), "abcdefgh", "referans")
	answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read")
	if answer.Access != "not_kept" || answer.Subject != nil || answer.PlainText != nil || answer.HTML != nil {
		t.Errorf("content = %+v, want not_kept with nothing in it", answer)
	}
	if n := countViews(t, db, task); n != 0 {
		t.Errorf("openings recorded = %d, want 0", n)
	}
}

// A sensitive send the sweep emptied within the retention period — before
// sensitive content was kept — says so, not that it is secret or expired.
func TestSensitiveSendEmptiedBeforeItWasKeptIsNotKept(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
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
	if answer := contentAs(t, app, task, row, contentOther, "skymail:access", "skymail:mails:read"); answer.Access != "restricted" {
		t.Errorf("a reader: %+v, want restricted", answer)
	}

	no := false
	shown := keycloakLike("core.welcome", contentSender, now.Add(-48*time.Hour))
	shown.sensitive, shown.cleared = &no, true
	shownTask, shownRow := seedRenderedSend(t, db, shown)
	if answer := contentAs(t, app, shownTask, shownRow, contentOther, system...); answer.Access != "expired" {
		t.Errorf("emptied shown send: %+v, want expired", answer)
	}
}

// The database's lock and Go's are one list.
func TestTheDatabaseLocksTheSameKeysAsGo(t *testing.T) {
	db := lifecycleHandlerStore(t)
	keys := []string{
		"keycloak.verify-email", "keycloak.generic", "account.security-alert", "auth.x", "login.y", "otp.z",
		"core.contact-consent-confirm", "event.ticket-created", "event.reminder", "event.ticket-issued",
		"core.email-change-link", "place.sign-in-link", "core.log-in", "forms.sign-in", "place.one-time-link",
		"place.giris-baglantisi", "core.dogrulama-kodu", "club.davet", "forms.onay", "core.sifre", "x.pass-code",
		"core.welcome", "core.certificate", "club.team-membership", "forms.form-copy", "forms.status-changed",
		"mail.approval-requested", "ops.send-failed", "event.updated", "free.basic", "club.privilege-change",
		"core.codex", "a..login", "sign.in-x",
	}
	for _, key := range keys {
		var locked bool
		if err := db.Conn.QueryRow(context.Background(), `SELECT template_key_content_locked($1)`, key).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if want := ContentSensitiveLocked(&key); locked != want {
			t.Errorf("%s: database %v, Go %v", key, locked, want)
		}
	}
}

// One send whose masking keeps failing holds no other back, is tried once a
// run, and on its third failure — counted because other sends were masked —
// is emptied (not kept); a send of many rows is masked a chunk at a time (#63
// re-review R2, Ek 3 Z1).
func TestMaskingAFailingSendHoldsNoOtherBackAndIsEmptiedOnItsThirdFailure(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	ctx := context.Background()
	failing, failingRow := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", contentSender, now.Add(-2*time.Hour)))
	many, manyRow := seedRenderedSend(t, db, keycloakLike("keycloak.verify-email", contentSender, now.Add(-time.Hour)))
	// Four more recipients of the second send: five rows, masked two at a time.
	if _, err := db.Conn.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at)
		SELECT task_id, recipient_full_name, recipient_email, subject, body, body_html, status, created_at
		FROM mail_queue, generate_series(1, 4) WHERE id = $1`, manyRow); err != nil {
		t.Fatal(err)
	}
	// Writing the first send's masked mail fails, whatever the cause.
	if _, err := db.Conn.Exec(ctx, `
		CREATE FUNCTION refuse_mask() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'disk full'; END $$;
		CREATE TRIGGER refuse_mask BEFORE UPDATE ON mail_queue FOR EACH ROW
			WHEN (NEW.task_id = '`+failing.String()+`' AND NEW.subject <> '') EXECUTE FUNCTION refuse_mask();`); err != nil {
		t.Fatal(err)
	}

	first := maskNow(t, db)
	if first.Sends != 1 || first.Rows != 5 || first.Failed != 1 || first.NotKept != 0 {
		t.Fatalf("first run = %+v, want the second send's five rows masked and the first failed", first)
	}
	assertNoSecret(t, "the second send", keptContent(t, db, many), resetLink, loginCode)
	if answer := contentAs(t, app, many, manyRow, contentSender, "skymail:access", "skymail:mails:read"); answer.Access != "masked" {
		t.Errorf("second send: %+v, want masked", answer)
	}
	if answer := contentAs(t, app, failing, failingRow, contentSender, "skymail:access", "skymail:mails:read"); answer.Access != "pending" {
		t.Errorf("failing send after one failure: %+v, want pending", answer)
	}
	// Each later run has a new send to mask, which shows the fault is the
	// failing send's.
	time.Sleep(50 * time.Millisecond)
	seedRenderedSend(t, db, keycloakLike("keycloak.later-1", contentSender, now.Add(-30*time.Minute)))
	if second := maskNow(t, db); second.Sends != 1 || second.Failed != 1 || second.NotKept != 0 {
		t.Fatalf("second run = %+v, want the new send masked and one failure", second)
	}
	time.Sleep(50 * time.Millisecond)
	seedRenderedSend(t, db, keycloakLike("keycloak.later-2", contentSender, now.Add(-20*time.Minute)))
	third := maskNow(t, db)
	if third.Sends != 1 || third.Failed != 1 || third.NotKept != 1 {
		t.Fatalf("third run = %+v, want the new send masked and the failing send emptied", third)
	}
	assertNoSecret(t, "the failing send", keptContent(t, db, failing), resetLink, loginCode, "Merhaba")
	if answer := contentAs(t, app, failing, failingRow, contentSender, "skymail:access", "skymail:mails:read"); answer.Access != "not_kept" {
		t.Errorf("failing send after three failures: %+v, want not_kept", answer)
	}
	time.Sleep(50 * time.Millisecond)
	if fourth := maskNow(t, db); fourth != (mailer.MaskResult{}) {
		t.Errorf("fourth run = %+v, want nothing to do", fourth)
	}
}

// A send found not sensitive is looked at once; made sensitive later, by its
// template, it is looked at again and masked (#63 re-review R7).
func TestASendFoundNotSensitiveIsLookedAtAgainWhenItsTemplateChanges(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	no := false
	send := keycloakLike("core.welcome", contentSender, now.Add(-time.Hour))
	send.sensitive = &no
	task, row := seedRenderedSend(t, db, send)
	if result := maskNow(t, db); result.NotSensitive != 1 || result.Sends != 0 {
		t.Fatalf("first run = %+v, want one send found not sensitive", result)
	}
	if result := maskNow(t, db); result != (mailer.MaskResult{}) {
		t.Fatalf("second run = %+v, want nothing to look at", result)
	}
	if _, err := db.Conn.Exec(context.Background(), `
		UPDATE templates SET content_sensitive = true WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task); err != nil {
		t.Fatal(err)
	}
	if result := maskNow(t, db); result.Sends != 1 {
		t.Fatalf("after the template changed = %+v, want the send masked", result)
	}
	if answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read"); answer.Access != "masked" {
		t.Errorf("content = %+v, want masked", answer)
	}
	assertNoSecret(t, "kept", keptContent(t, db, task), resetLink, loginCode)
}

// Sends found not sensitive do not count toward a run's limit: a run that
// may mask one sensitive send masks it, however many shown sends come first
// (#63 Ek 2, Y2).
func TestShownSendsDoNotHoldAMaskingRunBack(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	no := false
	for i := 0; i < 3; i++ {
		shown := keycloakLike("core.welcome-"+strconv.Itoa(i), contentSender, now.Add(-3*time.Hour))
		shown.sensitive = &no
		seedRenderedSend(t, db, shown)
	}
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", contentSender, now.Add(-time.Hour)))
	result, err := mailer.MaskSensitiveContent(context.Background(), db, mailer.MaskOptions{MaxSends: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sends != 1 || result.NotSensitive != 3 {
		t.Fatalf("run = %+v, want three shown sends looked at and the sensitive one masked", result)
	}
	if answer := contentAs(t, app, task, row, contentSender, "skymail:access", "skymail:mails:read"); answer.Access != "masked" {
		t.Errorf("content = %+v, want masked", answer)
	}
}
