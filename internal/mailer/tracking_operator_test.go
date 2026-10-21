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
// template, under the system's link rule: a sensitive mail gets the pixel
// only, any other a click address only on the links its template writes out
// as they are. A link a variable fills is never rewritten, even in a mail to
// one person, where a person's send that asked (flag off) rewrites it.

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
		{"System", formsReplyTemplate, formsVariables(), []string{"https://yildizskylab.com/etkinlikler"}},
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

	// A group send of a System template, and of one whose variable link
	// every member has: tracked as core says; the variable link, common to
	// all, is not registered.
	for _, template := range []uuid.UUID{systemTemplate, formsReplyTemplate} {
		variables := `{}`
		if template == formsReplyTemplate {
			variables = formsVariables()
		}
		task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: "operator-sub", TemplateID: template, MailListID: &announcementList, BodyVariables: []byte(variables),
			Recipients: groupOf(), Tracking: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		rows := trackedRows(t, db, task)
		if !trackingRequested(t, db, task) || len(rows["ayse@example.com"].token) != tracking.RecipientTokenBytes ||
			rows["mehmet@example.com"].reason != tracking.SkipPreferenceOff || rows["adres@example.com"].reason != tracking.SkipNoAccount {
			t.Errorf("group send of %s: requested %v, rows %+v", template, trackingRequested(t, db, task), rows)
		}
		for _, url := range linkURLs(linksOf(t, db, task)) {
			if strings.Contains(url, "{{") || strings.Contains(url, "tek-kullanimlik") || strings.Contains(url, inviteKey) || strings.Contains(url, "form?e=") {
				t.Errorf("group send of %s registered %s, a link a variable fills", template, url)
			}
		}
	}

	// An internal list's addresses are not looked up: the send is tracked,
	// its recipients no_account.
	task, err := m.Enqueue(ctx, database.CreateMailTaskParams{
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

// The secret link in an operator's mail is never rewritten. A person's
// single mail of a template that may be shown goes out with the pixel and a
// click address on the link its template writes out only: the edit link a
// variable fills, the address with the invitation code in it and the link
// {{.Email}} fills reach the recipient byte for byte, and none of them is a
// mail_task_links address or part of a /t/ address. A sensitive mail goes
// out with the pixel alone.
func TestASecretLinkInAnOperatorsMailIsNeverRewritten(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	if _, err := db.Conn.Exec(context.Background(), `UPDATE templates SET system = false WHERE id = $1`, formsReplyTemplate); err != nil {
		t.Fatal(err)
	}
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, operatorTracking(tracking.ModeOn, coreSays(), ayseHasAnAccount(nil)))
	plainSMTP(m)
	ctx := context.Background()

	forms, err := m.EnqueueTrackedSingle(ctx, personTo(formsReplyTemplate, "ayse@example.com", "Ayşe Yılmaz", formsVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := m.EnqueueTrackedSingle(ctx, personTo(resetPasswordTemplate, "ayse@example.com", "Ayşe Yılmaz", resetVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	formsLinks := linksOf(t, db, forms)
	if len(formsLinks) != 1 || formsLinks[0].url != "https://yildizskylab.com/etkinlikler" {
		t.Fatalf("the operator's Forms mail registered %+v, want only the link its template writes out", formsLinks)
	}
	if links := linksOf(t, db, reset); len(links) != 0 {
		t.Errorf("the operator's reset mail registered %+v; a sensitive mail registers none", links)
	}
	var kept int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_task_links WHERE url LIKE '%' || $1 || '%' OR url LIKE '%' || $2 || '%' OR url LIKE '%' || $3 || '%'`,
		"gizli-sifirlama", "tek-kullanimlik", inviteKey).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("%d registered links carry a secret", kept)
	}

	sendAll(t, db, m, relay, forms)
	waitFor(t, "the reset mail sent", 20*time.Second, func() bool {
		return trackedRows(t, db, reset)["ayse@example.com"].status == "sent"
	})
	var formsMail, resetMail relayedMail
	for _, raw := range relay.taken() {
		got := parseRelayed(t, raw)
		switch {
		case strings.Contains(got.html, "Yanıtını düzenle"):
			formsMail = got
		case strings.Contains(got.html, "Parolanı sıfırla"):
			resetMail = got
		}
	}

	formsRow := trackedRows(t, db, forms)["ayse@example.com"]
	click := tracking.ClickURL(trackingBase, formsRow.token, formsLinks[0].token)
	if !strings.Contains(formsMail.html, tracking.PixelHTML(tracking.PixelURL(trackingBase, formsRow.token))) {
		t.Errorf("the operator's Forms mail has no pixel:\n%s", formsMail.html)
	}
	for _, want := range []string{
		`<a href="` + click + `">Etkinlikler</a>`,
		`<a href="` + editLink + `">Yanıtını düzenle</a>`,
		`<a href="https://forms.yildizskylab.com/f/abc?davet=` + inviteKey + `">Forma dön</a>`,
	} {
		if !strings.Contains(formsMail.html, want) {
			t.Errorf("the operator's Forms mail lacks %s:\n%s", want, formsMail.html)
		}
	}
	if n := strings.Count(formsMail.html, "/t/c/"); n != 1 {
		t.Errorf("the operator's Forms mail has %d click addresses, want the one", n)
	}

	resetRow := trackedRows(t, db, reset)["ayse@example.com"]
	pixel := tracking.PixelHTML(tracking.PixelURL(trackingBase, resetRow.token))
	if !strings.Contains(resetMail.html, pixel) || strings.Contains(resetMail.html, "/t/c/") {
		t.Errorf("the operator's reset mail: want the pixel and no click address:\n%s", resetMail.html)
	}
	if strings.Replace(resetMail.html, pixel, "", 1) != resetRow.bodyHTML {
		t.Errorf("the operator's reset mail is more than the kept mail and the pixel")
	}
	for _, secret := range []string{"gizli-sifirlama", "tek-kullanimlik", inviteKey} {
		for _, mail := range []relayedMail{formsMail, resetMail} {
			for _, line := range strings.Split(mail.html, "\n") {
				if strings.Contains(line, secret) && strings.Contains(line, trackingBase) {
					t.Errorf("a /t/ address carries %q: %s", secret, line)
				}
			}
		}
	}
}
