package mailer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Operator tracking (SKYMAIL_TRACKING_OPERATOR, Yusuf 2026-10-21): every send
// a person queues is tracked, with no box and no opting out, whatever its
// template. A sensitive mail gets the pixel only. Any other gets a click
// address on the links the send gives every recipient alike — written in the
// template, or in a variable of the send's own, such as the body an operator
// wrote (orchestrator, 2026-10-21: as Mailchimp, HubSpot and Dynamics count
// a campaign body's links) — and never on one that changes with the
// recipient (.Email, .FullName), in whole or in part, even in a mail to one
// person, where a person's send that asked (flag off) rewrites it.

// operatorTemplate is an operator's announcement: a free body, a link written
// out, links the recipient's address and name make, a link a send's variable
// fills, one with a send's variable in part, and one marked not to count.
var operatorTemplate = uuid.MustParse("72000000-0000-4000-8000-000000000001")

const operatorHTML = `<!DOCTYPE html><html><body style="margin:0">
<h1>{{.Heading}}</h1>
<div>{{safeHTML .BodyHtml}}</div>
<a href="https://skylab.example/etkinlik">Etkinlik</a>
<a href="https://skylab.example/form?e={{.Email}}">Sana özel</a>
<a href="https://skylab.example/u/{{.FullName}}/onay">Adına özel</a>
<a href="{{.Link}}">Gönderimin bağlantısı</a>
<a href="https://skylab.example/k/{{.Slug}}?ref=duyuru">Kısmi</a>
<a href="https://skylab.example/gizli" data-skymail-notrack>Sayılmaz</a>
</body></html>`

// operatorVariables is what the operator wrote once for every recipient: a
// body with a link of its own and one that tries to name the recipient
// ({{.Email}} in a variable is text, never a template action).
func operatorVariables() string {
	return `{"Heading":"GECEKODU","Link":"https://skylab.example/kayit","Slug":"gecekodu",` +
		`"BodyHtml":"<p><a href=\"https://skylab.example/govde\">Gövde</a> <a href=\"https://skylab.example/x?e={{.Email}}\">Gömme</a></p>"}`
}

// The links an operator's announcement counts clicks on, and the ones it
// never may.
var (
	operatorCounted = []string{"https://skylab.example/govde", "https://skylab.example/etkinlik", "https://skylab.example/kayit", "https://skylab.example/k/gecekodu?ref=duyuru"}
	operatorNever   = []string{"form?e=", "/u/", "/gizli", "@", "%40", "Ay%c5%9fe", "Ayşe", "skymail-probe"}
)

func operatorQueue(t *testing.T) *database.Store {
	t.Helper()
	db := systemQueue(t)
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, system, content_sensitive)
		VALUES ($1, 'club.gecekodu', 'Duyuru', '{{.Heading}}', $2, '{{.Heading}}', '', false, false)`,
		operatorTemplate, operatorHTML); err != nil {
		t.Fatal(err)
	}
	return db
}

// checkOperatorLinks says what of an operator's announcement send was
// registered: every link of operatorCounted, and none that names, or is made
// from, the recipient.
func checkOperatorLinks(t *testing.T, what string, links []registeredLink) {
	t.Helper()
	got := map[string]bool{}
	for _, l := range links {
		got[l.url] = true
		for _, never := range operatorNever {
			if strings.Contains(l.url, never) {
				t.Errorf("%s registered %s, a link that carries %q", what, l.url, never)
			}
		}
	}
	for _, want := range operatorCounted {
		if !got[want] {
			t.Errorf("%s did not register %s; registered %v", what, want, linkURLs(links))
		}
	}
}

func operatorTracking(operator tracking.Mode, prefs tracking.Preferences, accounts tracking.Accounts) mailer.Option {
	return mailer.WithTracking(mailer.Tracking{
		Config:      tracking.Config{Mode: tracking.ModeOn, Operator: operator, BaseURL: trackingBase, CoreURL: "http://core.invalid"},
		Preferences: prefs,
		Accounts:    accounts,
	})
}

func personTo(template uuid.UUID, email, name, variables string) database.CreateSingleMailTaskParams {
	arg := singleTo(template, email, name, variables)
	arg.SentBy = "operator-sub"
	arg.TrackingRequested = true
	return arg
}

func linkURLs(links []registeredLink) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.url
	}
	return out
}

// Off (the default), and on beside SKYMAIL_TRACKING off, nothing changes: a
// System or sensitive template is still never tracked as asked, and a
// person's single mail still rewrites the link a variable filled.
func TestOperatorTrackingIsOffUnlessBothFlagsAreOn(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	ctx := context.Background()
	off := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, operatorTracking(tracking.ModeOff, coreSays(), ayseHasAnAccount(nil)))
	if off.OperatorTrackingAlways() || off.TrackingApplies(ctx, systemTemplate, &announcementList) || off.TrackingApplies(ctx, sensitiveTemplate, &announcementList) {
		t.Error("operator tracking off: always, or a System or sensitive template tracked")
	}
	alone := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithTracking(mailer.Tracking{
		Config: tracking.Config{Mode: tracking.ModeOff, Operator: tracking.ModeOn},
	}))
	if alone.OperatorTrackingAlways() || alone.TrackingApplies(ctx, announcementTemplate, &announcementList) {
		t.Error("SKYMAIL_TRACKING_OPERATOR=on with SKYMAIL_TRACKING off tracks")
	}

	// A person's single mail that asked, as #85 made it: every link of the
	// one mail is registered, the one {{.Email}} fills included.
	task, err := off.EnqueueTrackedSingle(ctx, personTo(announcementTemplate, "ayse@example.com", "Ayşe Yılmaz", `{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	urls := strings.Join(linkURLs(linksOf(t, db, task)), " ")
	if !strings.Contains(urls, "https://skylab.example/form?e=ayse%40example.com") {
		t.Errorf("flag off: a person's single mail registered %s; want the link the variable filled too, as before", urls)
	}
	// A System template asked for is not tracked at all.
	task, err = off.EnqueueTrackedSingle(ctx, personTo(formsReplyTemplate, "ayse@example.com", "Ayşe Yılmaz", formsVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if trackingRequested(t, db, task) {
		t.Error("flag off: a person's send of a System template is tracked")
	}
}

// On, a person's send is tracked whatever its template, in the bulk lane:
// the one-person mail registers only the links its template writes out (the
// link {{.Email}} or a variable fills is not, though one mail has nothing to
// compare it with), a sensitive or a locked-key one only the pixel, a System
// template the written links. Keycloak is asked for the account of a person
// named by address whatever the template, core decides.
func TestEveryPersonsSendIsTrackedWithOperatorTracking(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	ctx := context.Background()
	prefs, accounts := coreSays(), ayseHasAnAccount(db)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, operatorTracking(tracking.ModeOn, prefs, accounts))
	if !m.OperatorTrackingAlways() || m.SystemTrackingAvailable() {
		t.Fatalf("always %v, system %v; want operator tracking on, system off", m.OperatorTrackingAlways(), m.SystemTrackingAvailable())
	}
	for _, template := range []uuid.UUID{announcementTemplate, systemTemplate, sensitiveTemplate, lockedKeyTemplate} {
		if !m.TrackingApplies(ctx, template, nil) || !m.TrackingApplies(ctx, template, &announcementList) {
			t.Errorf("template %s: TrackingApplies false with operator tracking on", template)
		}
	}

	for _, c := range []struct {
		name      string
		template  uuid.UUID
		variables string
		links     []string
	}{
		{"may be shown", announcementTemplate, `{}`, []string{"https://skylab.example/etkinlik?gun=1&saat=2"}},
		// The edit link and the invitation code are the send's variables,
		// the same for whoever it goes to: an operator typed them.
		{"System", formsReplyTemplate, formsVariables(), []string{"https://yildizskylab.com/etkinlikler", editLink, "https://forms.yildizskylab.com/f/abc?davet=" + inviteKey}},
		{"locked key", resetPasswordTemplate, resetVariables(), nil},
		{"sensitive", sensitiveTemplate, `{}`, nil},
	} {
		task, err := m.EnqueueTrackedSingle(ctx, personTo(c.template, "ayse@example.com", "Ayşe Yılmaz", c.variables), nil)
		if err != nil {
			t.Fatal(err)
		}
		row := trackedRows(t, db, task)["ayse@example.com"]
		if !trackingRequested(t, db, task) || laneOfTask(t, db, task) != mailer.PriorityBulk || len(row.token) != tracking.RecipientTokenBytes {
			t.Errorf("%s: requested %v, lane %d, token %x; want tracked in the bulk lane", c.name, trackingRequested(t, db, task), laneOfTask(t, db, task), row.token)
		}
		if sub := subOf(t, db, task); sub == nil || *sub != ayse {
			t.Errorf("%s: recipient_sub %v, want Ayşe's account", c.name, sub)
		}
		if got := linkURLs(linksOf(t, db, task)); strings.Join(got, " ") != strings.Join(c.links, " ") {
			t.Errorf("%s: links %v, want %v", c.name, got, c.links)
		}
	}
	if len(accounts.asked) != 4 || len(prefs.asked()) != 4 {
		t.Errorf("Keycloak asked %d times, core %d; want 4 and 4 (sensitive templates too)", len(accounts.asked), len(prefs.asked()))
	}
	for _, open := range append(append([]int{}, accounts.openTx...), prefs.openTx...) {
		if open != 0 {
			t.Fatalf("asked with a transaction open: Keycloak %v, core %v", accounts.openTx, prefs.openTx)
		}
	}

	// A group send of a System template: tracked as core says; the link
	// {{.Email}} makes is not registered, though written in the template.
	task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: "operator-sub", TemplateID: systemTemplate, MailListID: &announcementList, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := trackedRows(t, db, task)
	if !trackingRequested(t, db, task) || len(rows["ayse@example.com"].token) != tracking.RecipientTokenBytes ||
		rows["mehmet@example.com"].reason != tracking.SkipPreferenceOff || rows["adres@example.com"].reason != tracking.SkipNoAccount {
		t.Errorf("group send of a System template: requested %v, rows %+v", trackingRequested(t, db, task), rows)
	}
	if got := linkURLs(linksOf(t, db, task)); strings.Join(got, " ") != "https://skylab.example/etkinlik?gun=1&saat=2" {
		t.Errorf("group send of a System template registered %v", got)
	}

	// An internal list's addresses are not looked up: the send is tracked,
	// its recipients no_account.
	task, err = m.Enqueue(ctx, database.CreateMailTaskParams{
		SentBy: "operator-sub", TemplateID: &announcementTemplate, MailListID: &announcementList, BodyVariables: []byte(`{}`), TrackingRequested: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trackingRequested(t, db, task) {
		t.Error("a list send is not tracked with operator tracking on")
	}
	for email, row := range trackedRows(t, db, task) {
		if row.token != nil || row.reason != tracking.SkipNoAccount {
			t.Errorf("list send, %s: token %x, reason %q; want no_account", email, row.token, row.reason)
		}
	}

	// A service's single mail is the system's, not the operator's: untracked
	// while SKYMAIL_TRACKING_SYSTEM is off.
	service, tracked, err := m.EnqueueSystemSingle(ctx, singleTo(formsReplyTemplate, "ayse@example.com", "Ayşe", formsVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tracked || trackingRequested(t, db, service) || laneOfTask(t, db, service) != mailer.PrioritySystem {
		t.Errorf("a service's mail with only operator tracking on: tracked %v, requested %v", tracked, trackingRequested(t, db, service))
	}
	// Nor is a single mail queued without asking (a notice, a service's mail
	// with system tracking off): only the callers that queue a person's send
	// ask for every one.
	plain, err := m.EnqueueSingle(ctx, singleTo(formsReplyTemplate, "ayse@example.com", "Ayşe", formsVariables()))
	if err != nil {
		t.Fatal(err)
	}
	if trackingRequested(t, db, plain) {
		t.Error("EnqueueSingle tracked a mail with operator tracking on")
	}
}

// A link made from the recipient is never rewritten in an operator's mail.
// An announcement to one person (where there is no other mail to tell a
// personal link by) and to a group registers the body's link, the template's
// written link, the link a send's variable fills and the one with a send's
// variable in part; never the links .Email and .FullName make, the one marked
// data-skymail-notrack, nor anything naming the recipient — not even the
// {{.Email}} the body tried to slip in, which is text and stays text.
func TestALinkMadeFromTheRecipientIsNeverRewrittenInAnOperatorsMail(t *testing.T) {
	captureMailerLogs(t)
	db := operatorQueue(t)
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, operatorTracking(tracking.ModeOn, coreSays(), ayseHasAnAccount(nil)))
	ctx := context.Background()

	single, err := m.EnqueueTrackedSingle(ctx, personTo(operatorTemplate, "ayse@example.com", "Ayşe Yılmaz", operatorVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkOperatorLinks(t, "the mail to Ayşe", linksOf(t, db, single))
	group, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: "operator-sub", TemplateID: operatorTemplate, MailListID: &announcementList, BodyVariables: []byte(operatorVariables()),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkOperatorLinks(t, "the group send", linksOf(t, db, group))
	for _, task := range []uuid.UUID{single, group} {
		for email, row := range trackedRows(t, db, task) {
			if strings.Contains(row.bodyHTML, "skymail-probe") {
				t.Errorf("%s's kept mail carries a probe recipient", email)
			}
		}
	}

	// Flag off, the same mail to one person that asked registers the links
	// its recipient's address and name made, as #85 does.
	off := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, operatorTracking(tracking.ModeOff, coreSays(), ayseHasAnAccount(nil)))
	asked, err := off.EnqueueTrackedSingle(ctx, personTo(operatorTemplate, "ayse@example.com", "Ayşe Yılmaz", operatorVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if urls := strings.Join(linkURLs(linksOf(t, db, asked)), " "); !strings.Contains(urls, "form?e=ayse%40example.com") {
		t.Errorf("flag off: registered %s; want the link the address made too, as before", urls)
	}
}

// anchorHref is the href of the <a> whose text is text in html, as written.
func anchorHref(t *testing.T, html, text string) string {
	t.Helper()
	end := strings.Index(html, ">"+text+"</a>")
	open := strings.LastIndex(html[:max(end, 0)], "<a ")
	if end < 0 || open < 0 {
		t.Fatalf("no link %q in:\n%s", text, html)
	}
	tag := html[open:end]
	start := strings.Index(tag, `href="`)
	if start < 0 {
		t.Fatalf("link %q has no href: %s", text, tag)
	}
	value := tag[start+len(`href="`):]
	return value[:strings.Index(value, `"`)]
}

// Through the relay: the mail to one person goes out with the pixel, click
// addresses on the counted links, and every link made from the recipient,
// and the one marked, byte for byte as the kept mail has it; no /t/ address
// names the recipient. A sensitive mail of the operator's goes out with the
// pixel alone.
func TestAnOperatorsMailGoesOutWithItsPersonalLinksAsTheyAre(t *testing.T) {
	captureMailerLogs(t)
	db := operatorQueue(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, operatorTracking(tracking.ModeOn, coreSays(), ayseHasAnAccount(nil)))
	plainSMTP(m)
	ctx := context.Background()

	announcement, err := m.EnqueueTrackedSingle(ctx, personTo(operatorTemplate, "ayse@example.com", "Ayşe Yılmaz", operatorVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := m.EnqueueTrackedSingle(ctx, personTo(resetPasswordTemplate, "ayse@example.com", "Ayşe Yılmaz", resetVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if links := linksOf(t, db, reset); len(links) != 0 {
		t.Errorf("the operator's reset mail registered %+v; a sensitive mail registers none", links)
	}

	sendAll(t, db, m, relay, announcement)
	waitFor(t, "the reset mail sent", 20*time.Second, func() bool {
		return trackedRows(t, db, reset)["ayse@example.com"].status == "sent"
	})
	var mail, resetMail relayedMail
	for _, raw := range relay.taken() {
		got := parseRelayed(t, raw)
		switch {
		case strings.Contains(got.html, "Sana özel"):
			mail = got
		case strings.Contains(got.html, "Parolanı sıfırla"):
			resetMail = got
		}
	}

	row := trackedRows(t, db, announcement)["ayse@example.com"]
	if !strings.Contains(mail.html, tracking.PixelHTML(tracking.PixelURL(trackingBase, row.token))) {
		t.Errorf("the announcement has no pixel:\n%s", mail.html)
	}
	for _, text := range []string{"Sana özel", "Adına özel", "Sayılmaz", "Gömme"} {
		kept, sent := anchorHref(t, row.bodyHTML, text), anchorHref(t, mail.html, text)
		if text == "Gömme" {
			// Text an operator wrote once for all: counted, but naming no one.
			if strings.Contains(sent, "ayse") || strings.Contains(kept, "ayse") {
				t.Errorf("the body's {{.Email}} became the recipient's address: kept %s, sent %s", kept, sent)
			}
			continue
		}
		if sent != kept || strings.Contains(sent, "/t/c/") {
			t.Errorf("%q went out as %s; kept as %s, want it as it is", text, sent, kept)
		}
	}
	for _, text := range []string{"Gövde", "Etkinlik", "Gönderimin bağlantısı", "Kısmi"} {
		if href := anchorHref(t, mail.html, text); !strings.HasPrefix(href, trackingBase+"/t/c/") {
			t.Errorf("%q went out as %s, want a click address", text, href)
		}
	}
	for _, line := range strings.Split(mail.html, "\n") {
		if strings.Contains(line, trackingBase) && (strings.Contains(line, "ayse") || strings.Contains(line, "Ay%c5%9fe")) {
			t.Errorf("a /t/ address names the recipient: %s", line)
		}
	}

	resetRow := trackedRows(t, db, reset)["ayse@example.com"]
	pixel := tracking.PixelHTML(tracking.PixelURL(trackingBase, resetRow.token))
	if !strings.Contains(resetMail.html, pixel) || strings.Contains(resetMail.html, "/t/c/") {
		t.Errorf("the operator's reset mail: want the pixel and no click address:\n%s", resetMail.html)
	}
	if strings.Replace(resetMail.html, pixel, "", 1) != resetRow.bodyHTML {
		t.Errorf("the operator's reset mail is more than the kept mail and the pixel")
	}
	for _, line := range strings.Split(resetMail.html, "\n") {
		if strings.Contains(line, "gizli-sifirlama") && strings.Contains(line, trackingBase) {
			t.Errorf("a /t/ address carries the reset key: %s", line)
		}
	}
}
