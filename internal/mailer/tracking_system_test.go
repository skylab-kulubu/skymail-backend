package mailer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// System tracking (SKYMAIL_TRACKING_SYSTEM, Yusuf 2026-10-21): the mail
// services send without asking — Keycloak's and core's account and
// transaction mail, Forms' confirmations, core's notifications — is tracked
// too, in its own lane, whatever its template. A link that carries a secret
// (a password reset, an e-mail verification, a login link, an address with a
// sensitive variable) never goes through /t/: such a mail's links go to their
// address as they are, and the pixel is still there.

var (
	// keycloak.reset-password: a locked key, so sensitive whatever the row
	// says; a reset link from a variable and one written out.
	resetPasswordTemplate = uuid.MustParse("71000000-0000-4000-8000-000000000001")
	// forms.yanit-alindi: a System template that says it may be shown; a
	// link written out, one a variable fills, one with a variable in it.
	formsReplyTemplate = uuid.MustParse("71000000-0000-4000-8000-000000000002")
)

const resetHTML = `<!DOCTYPE html><html><body style="margin:0">
<p>Merhaba {{.FullName}},</p>
<a href="{{.link}}">Parolanı sıfırla</a>
<a href="https://yildizskylab.com/yardim">Yardım</a>
</body></html>`

const formsHTML = `<!DOCTYPE html><html><body style="margin:0">
<p>{{.FormTitle}} yanıtın alındı.</p>
<a href="https://yildizskylab.com/etkinlikler">Etkinlikler</a>
<a href="{{.EditUrl}}">Yanıtını düzenle</a>
<a href="https://forms.yildizskylab.com/f/abc?davet={{.Code}}">Forma dön</a>
</body></html>`

// The secrets the mails carry: they must reach the recipient as they are and
// never be a mail_task_links address or part of a /t/ address.
const (
	resetLink = "https://e.yildizskylab.com/realms/e-skylab/login-actions/action-token?key=eyJhbGciOiJIUzI1NiJ9.gizli-sifirlama&client_id=account"
	editLink  = "https://forms.yildizskylab.com/r/duzenle?token=tek-kullanimlik-9f3a"
	inviteKey = "davet-7Hq2"
)

// systemQueue is trackingQueue with the two service templates.
func systemQueue(t *testing.T) *database.Store {
	t.Helper()
	db := trackingQueue(t)
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, system, content_sensitive)
		VALUES ($1, 'keycloak.reset-password', 'Parola sıfırlama', 'Parola sıfırlama', $3, 'Sıfırla: {{.link}}', '', true, NULL),
		       ($2, 'forms.yanit-alindi', 'Yanıt alındı', 'Yanıtın alındı', $4, 'Düzenle: {{.EditUrl}}', '', true, false)`,
		resetPasswordTemplate, formsReplyTemplate, resetHTML, formsHTML); err != nil {
		t.Fatal(err)
	}
	return db
}

// fakeAccounts is Keycloak finding accounts by address: Ayşe has one; every
// search is counted with the transactions open when it ran.
type fakeAccounts struct {
	mu       sync.Mutex
	accounts map[string]uuid.UUID
	err      error
	delay    time.Duration
	asked    []string
	openTx   []int
	db       *database.Store
}

func (a *fakeAccounts) VerifiedAccountByEmail(ctx context.Context, address string) (*uuid.UUID, error) {
	open := 0
	if a.db != nil {
		if err := a.db.Conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.asked = append(a.asked, address)
	a.openTx = append(a.openTx, open)
	a.mu.Unlock()
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.err != nil {
		return nil, a.err
	}
	if sub, ok := a.accounts[address]; ok {
		return &sub, nil
	}
	return nil, nil
}

func ayseHasAnAccount(db *database.Store) *fakeAccounts {
	return &fakeAccounts{accounts: map[string]uuid.UUID{"ayse@example.com": ayse, "mehmet@example.com": mehmet}, db: db}
}

func systemTracking(system tracking.Mode, prefs tracking.Preferences, accounts tracking.Accounts) mailer.Option {
	return mailer.WithTracking(mailer.Tracking{
		Config:      tracking.Config{Mode: tracking.ModeOn, System: system, BaseURL: trackingBase, CoreURL: "http://core.invalid"},
		Preferences: prefs,
		Accounts:    accounts,
	})
}

func singleTo(template uuid.UUID, email, name, variables string) database.CreateSingleMailTaskParams {
	return database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(variables),
		RecipientFullName: name, RecipientEmail: email,
	}
}

func resetVariables() string {
	return `{"link":"` + resetLink + `"}`
}

func formsVariables() string {
	return `{"FormTitle":"Kayıt","EditUrl":"` + editLink + `","Code":"` + inviteKey + `"}`
}

func laneOfTask(t *testing.T, db *database.Store, task uuid.UUID) mailer.Priority {
	t.Helper()
	var lane int16
	if err := db.Conn.QueryRow(context.Background(), `SELECT max(priority) FROM mail_queue WHERE task_id = $1`, task).Scan(&lane); err != nil {
		t.Fatal(err)
	}
	return mailer.Priority(lane)
}

func subOf(t *testing.T, db *database.Store, task uuid.UUID) *uuid.UUID {
	t.Helper()
	var sub *uuid.UUID
	if err := db.Conn.QueryRow(context.Background(), `SELECT recipient_sub FROM mail_queue WHERE task_id = $1`, task).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	return sub
}

// Off by default: with SKYMAIL_TRACKING on but SKYMAIL_TRACKING_SYSTEM not,
// a service's mail is queued exactly as before — untracked, in the system
// lane, Keycloak and core not asked. SKYMAIL_TRACKING_SYSTEM on beside
// SKYMAIL_TRACKING off is off too.
func TestSystemTrackingIsOffUnlessBothFlagsAreOn(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	for _, c := range []struct {
		name   string
		config tracking.Config
	}{
		{"system off", tracking.Config{Mode: tracking.ModeOn, BaseURL: trackingBase, CoreURL: "http://core.invalid"}},
		{"tracking off", tracking.Config{Mode: tracking.ModeOff, System: tracking.ModeOn}},
	} {
		prefs, accounts := coreSays(), ayseHasAnAccount(nil)
		m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithTracking(mailer.Tracking{
			Config: c.config, Preferences: prefs, Accounts: accounts,
		}))
		if m.SystemTrackingAvailable() {
			t.Errorf("%s: SystemTrackingAvailable() = true", c.name)
		}
		if got := m.ResolveSystemTracking(context.Background(), groupOf()); got != nil {
			t.Errorf("%s: ResolveSystemTracking = %+v, want nil", c.name, got)
		}
		task, tracked, err := m.EnqueueSystemSingle(context.Background(), singleTo(formsReplyTemplate, "ayse@example.com", "Ayşe Yılmaz", formsVariables()), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tracked || trackingRequested(t, db, task) || laneOfTask(t, db, task) != mailer.PrioritySystem || subOf(t, db, task) != nil {
			t.Errorf("%s: tracked %v, requested %v, lane %d", c.name, tracked, trackingRequested(t, db, task), laneOfTask(t, db, task))
		}
		for email, row := range trackedRows(t, db, task) {
			if row.token != nil || row.reason != "" {
				t.Errorf("%s, %s: token %x, reason %q", c.name, email, row.token, row.reason)
			}
		}
		if len(linksOf(t, db, task)) != 0 || len(prefs.asked()) != 0 || len(accounts.asked) != 0 {
			t.Errorf("%s: links %v, core asked %v, Keycloak asked %v", c.name, linksOf(t, db, task), prefs.asked(), accounts.asked)
		}
	}
	if mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn).SystemTrackingAvailable() {
		t.Error("a mailer made without tracking tracks system mail")
	}
}

// On, a service's single mail stays in the system lane and is tracked like a
// person's: the recipient's account is the one Keycloak finds for the
// address, core decides, and both are asked before the send's transaction.
// Ayşe is tracked, Mehmet turned it off, an address with no account is
// no_account. A System template, and one whose key is locked sensitive, are
// tracked as well. EnqueueSystemSingle says tracked only for the recipient
// who got a token: the send is tracked (tracking_requested) for all three.
func TestAServicesSingleMailIsTrackedWithSystemTrackingOn(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	prefs, accounts := coreSays(), ayseHasAnAccount(db)
	prefs.db = db
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, systemTracking(tracking.ModeOn, prefs, accounts))
	if !m.SystemTrackingAvailable() {
		t.Fatal("SystemTrackingAvailable() = false with both flags on")
	}
	for _, c := range []struct {
		email, reason string
		sub           *uuid.UUID
		token         bool
	}{
		{"AYSE@example.com", "", &ayse, true},
		{"mehmet@example.com", tracking.SkipPreferenceOff, &mehmet, false},
		{"hesapsiz@example.com", tracking.SkipNoAccount, nil, false},
	} {
		for _, template := range []uuid.UUID{formsReplyTemplate, resetPasswordTemplate} {
			variables := formsVariables()
			if template == resetPasswordTemplate {
				variables = resetVariables()
			}
			task, tracked, err := m.EnqueueSystemSingle(context.Background(), singleTo(template, c.email, "Kişi", variables), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tracked != c.token || !trackingRequested(t, db, task) || laneOfTask(t, db, task) != mailer.PrioritySystem {
				t.Errorf("%s: tracked %v, requested %v, lane %d; want tracked %v, requested, in the system lane", c.email, tracked, trackingRequested(t, db, task), laneOfTask(t, db, task), c.token)
			}
			sub := subOf(t, db, task)
			if (sub == nil) != (c.sub == nil) || (sub != nil && *sub != *c.sub) {
				t.Errorf("%s: recipient_sub %v, want %v", c.email, sub, c.sub)
			}
			for _, row := range trackedRows(t, db, task) {
				if (len(row.token) == tracking.RecipientTokenBytes) != c.token || row.reason != c.reason || row.status != "pending" {
					t.Errorf("%s: %s, token %x, reason %q; want token %v, reason %q", c.email, row.status, row.token, row.reason, c.token, c.reason)
				}
			}
		}
	}
	if len(accounts.asked) != 6 || len(prefs.asked()) != 4 {
		t.Errorf("Keycloak asked %d times, core %d; want 6 and 4 (no account, nothing to ask core)", len(accounts.asked), len(prefs.asked()))
	}
	for _, open := range append(append([]int{}, accounts.openTx...), prefs.openTx...) {
		if open != 0 {
			t.Fatalf("asked with a transaction open: Keycloak %v, core %v", accounts.openTx, prefs.openTx)
		}
	}
	// A plain single mail is still untracked: only EnqueueSystemSingle and
	// the system send flag track by the system.
	task, err := m.EnqueueSingle(context.Background(), singleTo(formsReplyTemplate, "ayse@example.com", "Ayşe", formsVariables()))
	if err != nil {
		t.Fatal(err)
	}
	if trackingRequested(t, db, task) || trackedRows(t, db, task)["ayse@example.com"].token != nil {
		t.Error("EnqueueSingle tracked a mail with system tracking on")
	}
}

// The secret link is never rewritten. A password reset (a key locked
// sensitive) goes out with the pixel right after <body> and every link as it
// was: the reset link byte for byte, no /t/c address, nothing registered.
// A Forms confirmation that may be shown (one recipient, so CommonLinks would
// count every link) gets a click address on the link its template writes out
// only: the edit link a variable fills and the address with the invitation
// code in it go to the recipient as they are, and neither is kept in
// mail_task_links.
func TestASecretLinkIsNeverRewritten(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, systemTracking(tracking.ModeOn, coreSays(), ayseHasAnAccount(nil)))
	plainSMTP(m)
	ctx := context.Background()

	reset, _, err := m.EnqueueSystemSingle(ctx, singleTo(resetPasswordTemplate, "ayse@example.com", "Ayşe Yılmaz", resetVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	forms, _, err := m.EnqueueSystemSingle(ctx, singleTo(formsReplyTemplate, "ayse@example.com", "Ayşe Yılmaz", formsVariables()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if links := linksOf(t, db, reset); len(links) != 0 {
		t.Errorf("the reset mail registered links %+v; a sensitive mail registers none", links)
	}
	formsLinks := linksOf(t, db, forms)
	if len(formsLinks) != 1 || formsLinks[0].url != "https://yildizskylab.com/etkinlikler" {
		t.Fatalf("the Forms mail registered %+v, want only the link its template writes out", formsLinks)
	}
	var kept int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_task_links WHERE url LIKE '%' || $1 || '%' OR url LIKE '%' || $2 || '%' OR url LIKE '%' || $3 || '%'`,
		"gizli-sifirlama", "tek-kullanimlik", inviteKey).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("%d registered links carry a secret", kept)
	}

	sendAll(t, db, m, relay, reset)
	waitFor(t, "the Forms mail sent", 20*time.Second, func() bool {
		return trackedRows(t, db, forms)["ayse@example.com"].status == "sent"
	})
	var resetMail, formsMail relayedMail
	for _, raw := range relay.taken() {
		got := parseRelayed(t, raw)
		switch {
		case strings.Contains(got.html, "Parolanı sıfırla"):
			resetMail = got
		case strings.Contains(got.html, "Yanıtını düzenle"):
			formsMail = got
		}
	}

	resetRow := trackedRows(t, db, reset)["ayse@example.com"]
	pixel := tracking.PixelHTML(tracking.PixelURL(trackingBase, resetRow.token))
	if len(resetRow.token) != tracking.RecipientTokenBytes || !strings.Contains(resetMail.html, `<body style="margin:0">`+pixel) {
		t.Errorf("the reset mail has no pixel right after <body>:\n%s", resetMail.html)
	}
	if strings.Contains(resetMail.html, "/t/c/") {
		t.Errorf("the reset mail has a click address:\n%s", resetMail.html)
	}
	escapedReset := strings.ReplaceAll(resetLink, "&", "&amp;")
	for _, want := range []string{`<a href="` + escapedReset + `">Parolanı sıfırla</a>`, `<a href="https://yildizskylab.com/yardim">Yardım</a>`} {
		if !strings.Contains(resetMail.html, want) {
			t.Errorf("the reset mail lacks %s:\n%s", want, resetMail.html)
		}
	}
	if strings.Replace(resetMail.html, pixel, "", 1) != resetRow.bodyHTML {
		t.Errorf("the reset mail is more than the kept mail and the pixel")
	}

	formsRow := trackedRows(t, db, forms)["ayse@example.com"]
	click := tracking.ClickURL(trackingBase, formsRow.token, formsLinks[0].token)
	if !strings.Contains(formsMail.html, tracking.PixelHTML(tracking.PixelURL(trackingBase, formsRow.token))) {
		t.Errorf("the Forms mail has no pixel:\n%s", formsMail.html)
	}
	for _, want := range []string{
		`<a href="` + click + `">Etkinlikler</a>`,
		`<a href="` + editLink + `">Yanıtını düzenle</a>`,
		`<a href="https://forms.yildizskylab.com/f/abc?davet=` + inviteKey + `">Forma dön</a>`,
	} {
		if !strings.Contains(formsMail.html, want) {
			t.Errorf("the Forms mail lacks %s:\n%s", want, formsMail.html)
		}
	}
	if n := strings.Count(formsMail.html, "/t/c/"); n != 1 {
		t.Errorf("the Forms mail has %d click addresses, want the one", n)
	}
	for _, secret := range []string{"gizli-sifirlama", "tek-kullanimlik", inviteKey} {
		for _, mail := range []relayedMail{resetMail, formsMail} {
			for _, line := range strings.Split(mail.html, "\n") {
				if strings.Contains(line, secret) && strings.Contains(line, trackingBase) {
					t.Errorf("a /t/ address carries %q: %s", secret, line)
				}
			}
		}
	}
}

// core's notification (EnqueueWithRecipientsParams.System), in a caller's
// transaction with what ResolveSystemTracking said before it: each person
// core allows is tracked, in the lane the caller chose; the link the
// template writes out is registered, the one a variable fills is not, though
// every recipient has it.
func TestASystemSendToManyTracksAsCoreSays(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	prefs := coreSays()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, systemTracking(tracking.ModeOn, prefs, ayseHasAnAccount(nil)))
	ctx := context.Background()
	group := groupOf()
	asked := m.ResolveSystemTracking(ctx, group)
	for _, lane := range []mailer.Priority{mailer.PrioritySystem, mailer.PriorityBulk} {
		var task uuid.UUID
		if err := db.InTx(ctx, func(q *database.Queries) error {
			var err error
			task, err = m.Queue(q, lane).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
				SentBy: laneSender, TemplateID: formsReplyTemplate, BodyVariables: []byte(formsVariables()),
				Recipients: group, System: true, Preferences: asked,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !trackingRequested(t, db, task) || laneOfTask(t, db, task) != lane {
			t.Errorf("lane %d: requested %v, lane %d", lane, trackingRequested(t, db, task), laneOfTask(t, db, task))
		}
		rows := trackedRows(t, db, task)
		if len(rows["ayse@example.com"].token) != tracking.RecipientTokenBytes {
			t.Errorf("lane %d: Ayşe not tracked: %+v", lane, rows["ayse@example.com"])
		}
		for email, reason := range map[string]string{
			"mehmet@example.com": tracking.SkipPreferenceOff,
			"zeynep@example.com": tracking.SkipNotMember,
			"adres@example.com":  tracking.SkipNoAccount,
		} {
			if rows[email].token != nil || rows[email].reason != reason {
				t.Errorf("lane %d, %s: token %x, reason %q; want %q", lane, email, rows[email].token, rows[email].reason, reason)
			}
		}
		if links := linksOf(t, db, task); len(links) != 1 || links[0].url != "https://yildizskylab.com/etkinlikler" {
			t.Errorf("lane %d: links %+v, want only the written one", lane, links)
		}
	}
	if len(prefs.asked()) != 1 {
		t.Errorf("core asked %d times, want once (before the transactions)", len(prefs.asked()))
	}

	// System off, the same send is not tracked whatever Tracking says.
	off := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, systemTracking(tracking.ModeOff, prefs, nil))
	task, err := off.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: group, System: true, Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if trackingRequested(t, db, task) || len(prefs.asked()) != 1 {
		t.Errorf("a system send with system tracking off: requested %v, core asked %d times", trackingRequested(t, db, task), len(prefs.asked()))
	}
}

// Someone waits for a login code: Keycloak and core together get 2 seconds.
// A Keycloak or a core that does not answer in time leaves the person
// untracked (preference_unavailable), and the mail goes out.
func TestAServicesMailGivesTrackingTwoSeconds(t *testing.T) {
	captureMailerLogs(t)
	db := systemQueue(t)
	for _, c := range []struct {
		name     string
		accounts *fakeAccounts
		core     tracking.Preferences
	}{
		{"slow Keycloak", &fakeAccounts{delay: time.Minute}, coreSays()},
		{"Keycloak down", &fakeAccounts{err: errors.New("keycloak down")}, coreSays()},
		{"slow core", ayseHasAnAccount(nil), &slowCore{}},
	} {
		m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, systemTracking(tracking.ModeOn, c.core, c.accounts))
		start := time.Now()
		task, tracked, err := m.EnqueueSystemSingle(context.Background(), singleTo(resetPasswordTemplate, "ayse@example.com", "Ayşe", resetVariables()), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if waited := time.Since(start); waited > 3*time.Second {
			t.Errorf("%s: the mail waited %v to be queued", c.name, waited)
		}
		row := trackedRows(t, db, task)["ayse@example.com"]
		if tracked || !trackingRequested(t, db, task) || row.token != nil || row.reason != tracking.SkipPreferenceUnavailable || row.status != "pending" {
			t.Errorf("%s: tracked %v, %s, token %x, reason %q; want pending, untracked, preference_unavailable", c.name, tracked, row.status, row.token, row.reason)
		}
		if core, ok := c.core.(*slowCore); ok && (!core.deadline || core.budget > 2*time.Second) {
			t.Errorf("core was given %s, want at most 2 seconds", core.budget)
		}
	}
}
