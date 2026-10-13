package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

type sensitivityEvent struct {
	Actor struct {
		Kind string  `json:"kind"`
		Sub  *string `json:"sub"`
		Name *string `json:"name"`
	} `json:"actor"`
	OldValue *bool `json:"old_value"`
	NewValue *bool `json:"new_value"`
}

func sensitivityEvents(t *testing.T, app *fiber.App, id uuid.UUID) []sensitivityEvent {
	t.Helper()
	var events []sensitivityEvent
	getJSON(t, app, "/templates/"+id.String()+"/content-sensitive/events", &events)
	return events
}

func setSensitive(t *testing.T, app *fiber.App, id uuid.UUID, value any) (int, servedTemplate, []byte) {
	t.Helper()
	response, body := sendJSON(t, app, fiber.MethodPut, "/templates/"+id.String()+"/content-sensitive", map[string]any{"content_sensitive": value})
	var served servedTemplate
	if response.StatusCode == fiber.StatusOK {
		assertTemplateShape(t, body)
		if err := json.Unmarshal(body, &served); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, served, body
}

func isBool(value *bool, want bool) bool { return value != nil && *value == want }

// The panel sets a template's content_sensitive either way, and each change
// is kept with who made it, before and after; setting what it is records
// nothing.
func TestPanelSetsContentSensitiveEitherWayAndRecordsWho(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	template := seededTemplate(t, app, "club.team-membership")

	status, served, body := setSensitive(t, app, template.ID, true)
	if status != fiber.StatusOK || !isBool(served.ContentSensitive, true) || served.ContentSensitiveLocked {
		t.Fatalf("set true = %d %s", status, body)
	}
	if status, served, body = setSensitive(t, app, template.ID, false); status != fiber.StatusOK || !isBool(served.ContentSensitive, false) {
		t.Fatalf("set false = %d %s", status, body)
	}
	if status, _, body = setSensitive(t, app, template.ID, false); status != fiber.StatusOK {
		t.Fatalf("set false again = %d %s", status, body)
	}
	if status, _, body = setSensitive(t, app, template.ID, nil); status != fiber.StatusBadRequest {
		t.Fatalf("set null = %d %s, want 400", status, body)
	}

	events := sensitivityEvents(t, app, template.ID)
	if len(events) != 2 {
		t.Fatalf("%d events, want 2 (the repeat records nothing): %+v", len(events), events)
	}
	newest, first := events[0], events[1]
	if !isBool(newest.OldValue, true) || !isBool(newest.NewValue, false) ||
		first.OldValue != nil || !isBool(first.NewValue, true) {
		t.Errorf("events %+v, want null→true then true→false, newest first", events)
	}
	for _, event := range events {
		if event.Actor.Kind != "operator" || event.Actor.Sub == nil || *event.Actor.Sub != operatorSub ||
			event.Actor.Name == nil || *event.Actor.Name != operatorName {
			t.Errorf("event actor %+v, want the operator", event.Actor)
		}
	}

	// An archived template is not set.
	if _, err := db.Conn.Exec(context.Background(), `UPDATE templates SET archived_at = now() WHERE id = $1`, template.ID); err != nil {
		t.Fatal(err)
	}
	if status, _, body = setSensitive(t, app, template.ID, true); status != fiber.StatusNotFound {
		t.Errorf("archived = %d %s, want 404", status, body)
	}
}

// A template whose mail carries a credential stays hidden: the panel cannot
// show it, the seed's false is written true, and a write that gives a template
// such a key marks it.
func TestCredentialTemplatesAreLockedSensitive(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)

	for _, key := range []string{
		// Every credential mail that sends to SkyMail today.
		"keycloak.verify-email", "keycloak.reset-password", "keycloak.update-email", "keycloak.idp-link",
		"keycloak.personal-email-confirm", "keycloak.generic",
		"account.security-alert", "account.primary-email-changed", "account.deletion-requested", "account.deletion-completed",
		"core.contact-consent-confirm", "event.ticket-created", "event.reminder",
		// Flows that may move here: a mail-and-code sign-in, a login link, a reset.
		"auth.email-code", "login.link", "otp.mail", "place.login-link", "core.login-code", "forms.guest-verify-email",
		"core.magic-link", "core.password-reset", "club.invite", "forms.email-otp",
		// D2: a ticket or a link, and sign-in / log-in written with a dash.
		"event.ticket-issued", "core.email-change-link", "place.sign-in-link", "core.log-in", "forms.sign-in",
		"place.one-time-link", "core.signin-code",
	} {
		k := key
		if !ContentSensitiveLocked(&k) {
			t.Errorf("%s is not locked", key)
		}
	}
	for _, key := range []string{
		// Shown: what sends to SkyMail today without a credential.
		"free.basic", "core.welcome", "core.certificate", "club.team-membership", "event.updated", "event.cancelled",
		"mail.approval-requested", "mail.approval-resolved", "ops.send-failed",
		"forms.form-copy", "forms.status-changed", "forms.pending-reminder", "forms.attempt-update",
		"keycloak", "accounts.x", "core.codex-news", "club.resetting", "core.signal",
	} {
		k := key
		if ContentSensitiveLocked(&k) {
			t.Errorf("%s is locked", key)
		}
	}
	if ContentSensitiveLocked(nil) {
		t.Error("a template without a key is locked")
	}

	// The seed asks for false: it is written true, and served as locked.
	payload := welcomeSeed("<p>{{.link}}</p>")
	payload["content_sensitive"] = false
	response, seeded, body := seedAs(t, app, "keycloak.reset-password", payload, false)
	if response.StatusCode != fiber.StatusOK || !isBool(seeded.ContentSensitive, true) {
		t.Fatalf("seed false on a locked key = %d %s, want true", response.StatusCode, body)
	}
	if served := getTemplate(t, app, "/templates/"+seeded.ID.String()); !served.ContentSensitiveLocked {
		t.Error("served without content_sensitive_locked")
	}
	if events := sensitivityEvents(t, app, seeded.ID); len(events) != 1 || events[0].Actor.Kind != "template_seed" || !isBool(events[0].NewValue, true) {
		t.Errorf("seed events %+v, want the seed's null→true", events)
	}

	status, _, body := setSensitive(t, app, seeded.ID, false)
	var refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if status != fiber.StatusConflict || refusal.Code != "template.content_sensitive_locked" || refusal.Params["key"] != "keycloak.reset-password" {
		t.Fatalf("panel false on a locked key = %d %s, want 409 template.content_sensitive_locked", status, body)
	}
	if status, served, body := setSensitive(t, app, seeded.ID, true); status != fiber.StatusOK || !isBool(served.ContentSensitive, true) {
		t.Fatalf("panel true on a locked key = %d %s", status, body)
	}

	// An operator's template given a locked key is marked, on create and on edit.
	response, body = sendJSON(t, app, fiber.MethodPost, "/templates", map[string]any{
		"name": "Kod", "subject": "Kod", "html_content": "<p>x</p>", "plain_text_content": "x", "react_email_content": "{}", "key": "account.login-code",
	})
	var created servedTemplate
	if err := json.Unmarshal(body, &created); response.StatusCode != fiber.StatusCreated || err != nil || !isBool(created.ContentSensitive, true) {
		t.Fatalf("create with a locked key = %d %s, want content_sensitive true", response.StatusCode, body)
	}
	response, body = sendJSON(t, app, fiber.MethodPost, "/templates", map[string]any{
		"name": "Bülten", "subject": "Bülten", "html_content": "<p>x</p>", "plain_text_content": "x", "react_email_content": "{}",
	})
	var plain servedTemplate
	if err := json.Unmarshal(body, &plain); response.StatusCode != fiber.StatusCreated || err != nil || plain.ContentSensitive != nil {
		t.Fatalf("create without a key = %d %s, want content_sensitive null", response.StatusCode, body)
	}
	response, body = sendJSON(t, app, fiber.MethodPatch, "/templates/"+plain.ID.String(), map[string]any{
		"name": "Bülten", "subject": "Bülten", "html_content": "<p>x</p>", "plain_text_content": "x", "react_email_content": "{}", "key": "event.reminder",
	})
	var edited servedTemplate
	if err := json.Unmarshal(body, &edited); response.StatusCode != fiber.StatusOK || err != nil || !isBool(edited.ContentSensitive, true) {
		t.Fatalf("edit to a locked key = %d %s, want content_sensitive true", response.StatusCode, body)
	}
}

// A locked template's sends are sensitive, so masked, even if its row says
// otherwise (a hand edit, a restored dump): the list is read when content is
// asked for.
func TestLockedTemplateContentIsMaskedWhateverItsRowSays(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	no := false
	task, row := seedContentSend(t, db, contentSend{sensitive: &no, list: true, sentBy: contentSender, createdAt: now.Add(-time.Hour)})
	if _, err := db.Conn.Exec(context.Background(), `
		UPDATE templates SET key = 'keycloak.reset-password'
		WHERE id = (SELECT template_id FROM mail_tasks WHERE id = $1)`, task); err != nil {
		t.Fatal(err)
	}
	maskNow(t, db)
	if answer := contentAs(t, app, task, row, contentOther, "skymail:access", "skymail:mails:read", SystemMailsReadRole); answer.Access != "masked" || answer.MaskedBy == nil {
		t.Errorf("content = %+v, want masked", answer)
	}
	status, body := getAs(t, app, "/mail_tasks/"+task.String(), contentSender, "skymail:access", "skymail:mails:read")
	var send struct {
		ContentAccess string `json:"content_access"`
	}
	if err := json.Unmarshal(body, &send); status != fiber.StatusOK || err != nil || send.ContentAccess != "masked" {
		t.Errorf("send = %d %s, want content_access masked", status, body)
	}
}

// A new single-mail template nobody has said anything about is sensitive:
// content_sensitive NULL leaves a single mail to its kind of send. Its code,
// one-time password, token, link or URL is masked, whatever the variable is
// called among these, in the subject, the plain text and the HTML.
func TestANewSingleTemplateCarryingACodeOrALinkIsMaskedByDefault(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
	app := contentApp(t, db, now)
	system := []string{"skymail:access", "skymail:mails:read", "skymail:mails:write", SystemMailsReadRole}
	values := map[string]string{
		"code":  "583014",
		"otp":   "771290",
		"token": "tok_9f8e7d6c5b4a39281706",
		"link":  "https://e.yildizskylab.com/verify?key=Zm9vYmFyMTIzNDU2Nzg5",
		"url":   "https://place.yildizskylab.com/magic?t=QmFyQmF6OTg3NjU0MzIx",
	}
	for i, variable := range []string{"code", "otp", "token", "link", "url"} {
		key := "core.new-mail-" + strconv.Itoa(i)
		if ContentSensitiveLocked(&key) {
			t.Fatalf("%s is locked; the test needs a key that is not", key)
		}
		task, row := seedRenderedSend(t, db, renderedSend{
			key:       key,
			subject:   "Kodun: {{." + variable + "}}",
			plainText: "{{." + variable + "}}",
			html:      `<p><a href="{{.` + variable + `}}">{{.` + variable + `}}</a></p>`,
			variables: map[string]any{variable: values[variable]},
			sentBy:    contentSender,
			createdAt: now.Add(-time.Hour),
		})
		maskNow(t, db)
		answer := contentAs(t, app, task, row, contentSender, system...)
		if answer.Access != "masked" || answer.Subject == nil || answer.HTML == nil || answer.PlainText == nil {
			t.Errorf("%s: content = %+v, want masked content", variable, answer)
			continue
		}
		assertNoSecret(t, variable, answer, values[variable])
	}
}

// A locked template that is not a system one keeps its key: renaming it, or
// sending no key, would take the lock off and open its past sends.
func TestALockedTemplatesKeyCannotBeChangedOrDropped(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := templateVersionsApp(t, db)
	payload := welcomeSeed("<p>{{.TicketCode}}</p>")
	payload["system"] = false
	response, seeded, body := seedAs(t, app, "event.ticket-created", payload, false)
	if response.StatusCode != fiber.StatusOK || seeded.System {
		t.Fatalf("seed = %d %s", response.StatusCode, body)
	}
	edit := func(key any) (int, []byte) {
		fields := map[string]any{
			"name": "Bilet", "subject": "Bilet", "html_content": "<p>{{.TicketCode}}</p>", "plain_text_content": "x", "react_email_content": "{}",
		}
		if key != nil {
			fields["key"] = key
		}
		response, body := sendJSON(t, app, fiber.MethodPatch, "/templates/"+seeded.ID.String(), fields)
		return response.StatusCode, body
	}
	for name, key := range map[string]any{"renamed": "event.ticket-x", "dropped": nil} {
		status, body := edit(key)
		var refusal struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &refusal)
		if status != fiber.StatusConflict || refusal.Code != "template.sensitive_key_immutable" {
			t.Errorf("%s: PATCH = %d %s, want 409 template.sensitive_key_immutable", name, status, body)
		}
	}
	if served := getTemplate(t, app, "/templates/"+seeded.ID.String()); served.Key == nil || *served.Key != "event.ticket-created" {
		t.Fatalf("key = %v, want it kept", served.Key)
	}
	// The same key: an edit like any other.
	if status, body := edit("event.ticket-created"); status != fiber.StatusOK {
		t.Errorf("PATCH with the same key = %d %s, want 200", status, body)
	}
}
