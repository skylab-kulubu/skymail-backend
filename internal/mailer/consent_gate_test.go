package mailer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/consent"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// fakeConsents is core's consent audience and address directory.
type fakeConsents struct {
	mu        sync.Mutex
	audiences map[string][]consent.Item
	accounts  map[string]bool
	down      bool
	// resolveDown fails recipients:resolve alone.
	resolveDown bool
	asked       []string
}

func (f *fakeConsents) Audience(_ context.Context, purpose string) ([]consent.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "audience "+purpose)
	if f.down {
		return nil, errors.New("core answered 503")
	}
	return f.audiences[purpose], nil
}

func (f *fakeConsents) Matched(_ context.Context, category string, addresses []string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "resolve "+category+" "+strings.Join(addresses, ","))
	if f.down || f.resolveDown {
		return nil, errors.New("core answered 503")
	}
	matched := map[string]bool{}
	for _, address := range addresses {
		matched[address] = f.accounts[address]
	}
	return matched, nil
}

func (f *fakeConsents) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

const (
	ayseWithdraw  = "https://api.example.org/v1/consents/withdraw?token=ayse"
	denizWithdraw = "https://api.example.org/v1/consents/withdraw?token=deniz"
)

func promotionalAudience() *fakeConsents {
	return &fakeConsents{audiences: map[string][]consent.Item{
		consent.PurposePromotional: {
			{Email: "ayse@example.com", TextVersion: "tanitim-v1", WithdrawURL: ayseWithdraw},
			{Email: "ayse@example.com", TextVersion: "tanitim-v1", WithdrawURL: "https://api.example.org/v1/consents/withdraw?token=second"},
			// A link List-Unsubscribe cannot carry: no consent for SkyMail.
			{Email: "baris@example.com", TextVersion: "tanitim-v1", WithdrawURL: "http://api.example.org/v1/consents/withdraw?token=baris"},
			{Email: "deniz@example.com", TextVersion: "tanitim-v1", WithdrawURL: denizWithdraw},
		},
	}}
}

func gatedMailer(t *testing.T, db *database.Store, gate mailer.ConsentGate, opts ...mailer.Option) mailer.Transactional {
	t.Helper()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, append(opts, mailer.WithConsentGate(gate))...)
	mailer.PrepareTurns(m, 10)
	return m
}

func duyuruSend(commercialAbsent bool) mailer.SendPurpose {
	purpose, category := mailer.PurposeDuyuru, "club:duyuru"
	return mailer.SendPurpose{Purpose: &purpose, Category: &category, CommercialContentAbsent: &commercialAbsent}
}

func enqueuePurposed(t *testing.T, m mailer.Transactional, purpose mailer.SendPurpose, recipients ...mailer.RecipientInfo) uuid.UUID {
	t.Helper()
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Recipients: recipients, Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func takenTo(rows []database.MailQueue) map[string]database.MailQueue {
	taken := map[string]database.MailQueue{}
	for _, row := range rows {
		taken[row.RecipientEmail] = row
	}
	return taken
}

func storedLink(t *testing.T, db *database.Store, task uuid.UUID, email string) string {
	t.Helper()
	var link *string
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT list_unsubscribe_url FROM mail_queue WHERE task_id = $1 AND recipient_email = $2`, task, email).Scan(&link); err != nil {
		t.Fatal(err)
	}
	if link == nil {
		return ""
	}
	return *link
}

// A promotional mail (s6 §5.10) goes only to an address in core's
// promotional audience, with that grant's withdraw link as its
// List-Unsubscribe; the others are written failed with nothing tried and
// never counted in the send budget. A duyuru with a sponsor's thanks (not
// promotional) goes to everyone, and core is not asked about it.
func TestAPromotionalMailGoesOnlyToThoseWhoConsented(t *testing.T) {
	logs := captureMailerLogs(t)
	db := suppressionQueue(t)
	core := promotionalAudience()
	clock := newTestClock()
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, Core: core}, mailer.WithRateBudget(mailer.RateBudget{PerMinute: 25}))
	mailer.SetClock(m, clock.now)

	ayse := mailer.RecipientInfo{FullName: "Ayşe", Email: "Ayse@Example.com"}
	baris := mailer.RecipientInfo{FullName: "Barış", Email: "baris@example.com"}
	zeynep := mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"}
	thanks := enqueuePurposed(t, m, duyuruSend(true), ayse, baris, zeynep)
	if taken := takenTo(mailer.Turn(context.Background(), m)); len(taken) != 3 {
		t.Fatalf("the sponsor's thanks: taken %d, want all three", len(taken))
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v about a mail that is not promotional", asked)
	}
	if link := storedLink(t, db, thanks, "zeynep@example.com"); link != "" {
		t.Errorf("the thanks got a link %q", link)
	}

	promo := enqueuePurposed(t, m, duyuruSend(false), ayse, baris, zeynep)
	taken := takenTo(mailer.Turn(context.Background(), m))
	if len(taken) != 1 || taken["Ayse@Example.com"].ListUnsubscribeUrl == nil || *taken["Ayse@Example.com"].ListUnsubscribeUrl != ayseWithdraw {
		t.Fatalf("taken %+v, want Ayşe alone with her withdraw link", taken)
	}
	if link := storedLink(t, db, promo, "Ayse@Example.com"); link != ayseWithdraw {
		t.Errorf("Ayşe's row keeps %q, want her withdraw link", link)
	}
	for _, email := range []string{"baris@example.com", "zeynep@example.com"} {
		if got := rowTo(t, db, promo, email); got.status != "failed" || got.error != "suppressed: no_promotional_consent" || got.attempts != 0 {
			t.Errorf("%s: %+v, want failed with suppressed: no_promotional_consent and nothing tried", email, got)
		}
	}
	if status := sendStatus(t, db, promo); status != "sending" {
		t.Errorf("send status %q, want sending: the held are skipped, not failed", status)
	}
	if n := count(t, db, `SELECT coalesce(sum(taken), 0)::int FROM mail_send_windows WHERE lane = 0 AND span_seconds = 60`); n != 4 {
		t.Errorf("bulk rows counted %d, want the three thanks and Ayşe's", n)
	}
	logs.mu.Lock()
	logged := logs.buf.String()
	logs.mu.Unlock()
	if strings.Contains(strings.ToLower(logged), "zeynep") || strings.Contains(logged, "token=") {
		t.Errorf("the log names an address or a link:\n%s", logged)
	}
}

// A consent withdrawn after the mail was queued holds it: the audience is
// read again every turn.
func TestAPromotionalConsentWithdrawnAfterQueueingHoldsTheMail(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := promotionalAudience()
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, Core: core})
	promo := enqueuePurposed(t, m, duyuruSend(false), mailer.RecipientInfo{FullName: "Deniz", Email: "deniz@example.com"})

	core.mu.Lock()
	core.audiences[consent.PurposePromotional] = core.audiences[consent.PurposePromotional][:1]
	core.mu.Unlock()
	if taken := mailer.Turn(context.Background(), m); len(taken) != 0 {
		t.Errorf("taken %d rows after Deniz withdrew", len(taken))
	}
	if got := rowTo(t, db, promo, "deniz@example.com"); got.error != "suppressed: no_promotional_consent" {
		t.Errorf("Deniz: %+v", got)
	}
}

// Core not answering is no audience: a promotional mail goes to no one,
// whatever SKYMAIL_PROMOTIONAL says now, and without CORE_URL too.
func TestAPromotionalMailGoesNowhereWithoutCore(t *testing.T) {
	captureMailerLogs(t)
	for name, gate := range map[string]mailer.ConsentGate{
		"core down":                    {Promotional: true, Core: &fakeConsents{down: true}},
		"switched off after queueing":  {Core: &fakeConsents{down: true}},
		"no CORE_URL, switch left off": {},
	} {
		t.Run(name, func(t *testing.T) {
			db := suppressionQueue(t)
			m := gatedMailer(t, db, gate)
			promo := enqueuePurposed(t, m, duyuruSend(false), mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com"})
			if taken := mailer.Turn(context.Background(), m); len(taken) != 0 {
				t.Errorf("taken %d rows", len(taken))
			}
			if got := rowTo(t, db, promo, "ayse@example.com"); got.status != "failed" || got.error != "suppressed: consent_unavailable" || got.attempts != 0 {
				t.Errorf("Ayşe: %+v, want failed with suppressed: consent_unavailable", got)
			}
		})
	}
}

// consentList is an internal list of a member (an account), a guest who
// consented to announcements on davet-v2, a guest without consent, one
// whose consent was given on davet-v1 (invitations alone) and one due the
// renewal question. The names start with an ASCII letter: initialTemplate
// writes the first byte of the name.
func consentList(t *testing.T, db *database.Store) uuid.UUID {
	t.Helper()
	list := uuid.New()
	if _, err := db.Conn.Exec(context.Background(), `INSERT INTO mailing_lists (id, name) VALUES ($1, 'Çalıştay')`, list); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(context.Background(), `
		WITH people AS (
			INSERT INTO recipients (full_name, email)
			VALUES ('Uye', 'Uye@example.com'), ('Rizali', 'rizali@example.com'), ('Misafir', 'misafir@example.com'),
			       ('Eski', 'eski@example.com'), ('Yenilenecek', 'yenilenecek@example.com')
			RETURNING id)
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) SELECT $1, id FROM people`, list); err != nil {
		t.Fatal(err)
	}
	return list
}

func announcementCore() *fakeConsents {
	return &fakeConsents{
		accounts: map[string]bool{"uye@example.com": true},
		audiences: map[string][]consent.Item{consent.PurposeEventInvitations: {
			{Email: "rizali@example.com", TextVersion: "davet-v2", WithdrawURL: "https://api.example.org/v1/consents/withdraw?token=r"},
			{Email: "eski@example.com", TextVersion: "davet-v1", WithdrawURL: "https://api.example.org/v1/consents/withdraw?token=e"},
			{Email: "yenilenecek@example.com", TextVersion: "davet-v2", WithdrawURL: "https://api.example.org/v1/consents/withdraw?token=y", RenewalDue: true},
		}},
	}
}

func enqueueToList(t *testing.T, m mailer.Transactional, list uuid.UUID) uuid.UUID {
	t.Helper()
	purpose, category := mailer.PurposeDuyuru, "club:haber"
	task, err := m.Enqueue(context.Background(), database.CreateMailTaskParams{
		SentBy: laneSender, TemplateID: &initialTemplate, MailListID: &list, BodyVariables: []byte(`{}`),
		Purpose: &purpose, Category: &category,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// With SKYMAIL_LIST_CONSENT on, a list's duyuru goes to an address that is
// an account's and to one whose announcement consent is open on a text that
// covers announcements; any other is written failed, "duyuru rızası yok".
// Core is asked with the send's category. A Keycloak group's members and a
// mail to one person are not held.
func TestAListAnnouncementToAnAddressWithoutAnAccountNeedsTheConsent(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := announcementCore()
	m := gatedMailer(t, db, mailer.ConsentGate{ListConsent: true, Core: core})
	list := consentList(t, db)
	task := enqueueToList(t, m, list)

	taken := takenTo(mailer.Turn(context.Background(), m))
	if len(taken) != 2 || taken["Uye@example.com"].ID == uuid.Nil || taken["rizali@example.com"].ID == uuid.Nil {
		t.Fatalf("taken %v, want the member and the guest who consented", taken)
	}
	if taken["rizali@example.com"].ListUnsubscribeUrl != nil {
		t.Errorf("an announcement got the consent's withdraw link")
	}
	for _, email := range []string{"misafir@example.com", "eski@example.com", "yenilenecek@example.com"} {
		if got := rowTo(t, db, task, email); got.status != "failed" || got.error != "suppressed: no_announcement_consent" || got.attempts != 0 {
			t.Errorf("%s: %+v, want failed with suppressed: no_announcement_consent", email, got)
		}
	}
	asked := core.calls()
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "resolve club:haber ") || asked[1] != "audience event_invitations" {
		t.Errorf("core asked %v", asked)
	}

	sub := uuid.New()
	group := enqueuePurposed(t, m, duyuruSend(true), mailer.RecipientInfo{FullName: "Grup üyesi", Email: "misafir@example.com", Sub: &sub})
	single, err := m.EnqueueSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &initialTemplate, BodyVariables: []byte(`{}`),
		RecipientFullName: "Misafir", RecipientEmail: "misafir@example.com", Purpose: duyuruSend(true).Purpose, Category: duyuruSend(true).Category,
	})
	if err != nil {
		t.Fatal(err)
	}
	if taken := mailer.Turn(context.Background(), m); len(taken) != 2 {
		t.Errorf("taken %d, want the group member's and the single mail", len(taken))
	}
	if got := rowTo(t, db, group, "misafir@example.com"); got.error != "" {
		t.Errorf("the group member: %+v", got)
	}
	if got := rowTo(t, db, single, "misafir@example.com"); got.error != "" {
		t.Errorf("the single mail: %+v", got)
	}
}

// SKYMAIL_LIST_CONSENT off (the default): a list's mail goes to every
// address as before, and core is not asked.
func TestListConsentOffSendsAsBefore(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := announcementCore()
	m := gatedMailer(t, db, mailer.ConsentGate{Core: core})
	enqueueToList(t, m, consentList(t, db))
	if taken := mailer.Turn(context.Background(), m); len(taken) != 5 {
		t.Errorf("taken %d, want all five", len(taken))
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v", asked)
	}
}

// Core not answering: an address whose account core cannot confirm gets no
// announcement, unless the audience, read, holds its consent.
func TestAListAnnouncementWithoutCoreGoesOnlyWhereTheConsentIsKnown(t *testing.T) {
	captureMailerLogs(t)
	for name, core := range map[string]*fakeConsents{"core down": {down: true}, "resolve down": nil} {
		t.Run(name, func(t *testing.T) {
			if core == nil {
				core = announcementCore()
				core.resolveDown = true
			}
			db := suppressionQueue(t)
			m := gatedMailer(t, db, mailer.ConsentGate{ListConsent: true, Core: core})
			task := enqueueToList(t, m, consentList(t, db))
			taken := takenTo(mailer.Turn(context.Background(), m))
			want := map[string]string{"Uye@example.com": "suppressed: consent_unavailable", "misafir@example.com": "suppressed: consent_unavailable"}
			if core.resolveDown {
				if len(taken) != 1 || taken["rizali@example.com"].ID == uuid.Nil {
					t.Errorf("taken %v, want the guest whose consent core holds", taken)
				}
			} else {
				if len(taken) != 0 {
					t.Errorf("taken %v, want none", taken)
				}
				want["rizali@example.com"] = "suppressed: consent_unavailable"
			}
			for email, reason := range want {
				if got := rowTo(t, db, task, email); got.status != "failed" || got.error != reason {
					t.Errorf("%s: %+v, want %s", email, got, reason)
				}
			}
		})
	}
}

// SKYMAIL_PROMOTIONAL and SKYMAIL_LIST_CONSENT are off unless they say on;
// anything else stops startup.
func TestTheConsentSwitches(t *testing.T) {
	gate, err := mailer.ConsentGateFromEnv(getenv(map[string]string{}))
	if err != nil || gate.Promotional || gate.ListConsent {
		t.Errorf("unset: %+v %v, want both off", gate, err)
	}
	gate, err = mailer.ConsentGateFromEnv(getenv(map[string]string{"SKYMAIL_PROMOTIONAL": " On ", "SKYMAIL_LIST_CONSENT": "on"}))
	if err != nil || !gate.Promotional || !gate.ListConsent {
		t.Errorf("on: %+v %v", gate, err)
	}
	for _, name := range []string{"SKYMAIL_PROMOTIONAL", "SKYMAIL_LIST_CONSENT"} {
		if _, err := mailer.ConsentGateFromEnv(getenv(map[string]string{name: "yes"})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=yes: %v, want startup stopped", name, err)
		}
	}
}

// A promotional mail's footer line says what its link does: it withdraws
// the promotional consent.
func TestThePromotionalFooterLine(t *testing.T) {
	if got := mailer.PlainWithUnsubscribeFooter("Metin", ayseWithdraw); got != "Metin\n\nTanıtım içerikli e-postaları almak istemiyorsan: İzni geri al <"+ayseWithdraw+">\n" {
		t.Errorf("plain %q", got)
	}
	if got := mailer.HTMLWithUnsubscribeFooter("<p>x</p>", ayseWithdraw); !strings.Contains(got, ">İzni geri al</a>") {
		t.Errorf("html %q", got)
	}
	if got := mailer.PlainWithUnsubscribeFooter("Metin", "https://x.example.org/u/a"); !strings.Contains(got, "Abonelikten çık") {
		t.Errorf("a duyuru link's line %q", got)
	}
}

// SKYMAIL_PROMOTIONAL=on needs purposes, List-Unsubscribe and a duyuru lane
// that writes it; anything less stops startup. Off asks for nothing.
func TestPromotionalNeedsItsWayOut(t *testing.T) {
	graph := map[mailer.Lane]mailer.LaneConfig{mailer.LaneDuyuru: {Transport: mailer.TransportGraph}}
	off := map[mailer.Lane]mailer.LaneConfig{mailer.LaneDuyuru: {Transport: mailer.TransportOff}}
	for _, tc := range []struct {
		name  string
		gate  mailer.ConsentGate
		lanes mailer.SendLanes
		want  string
	}{
		{"off", mailer.ConsentGate{}, mailer.SendLanes{}, ""},
		{"list consent alone", mailer.ConsentGate{ListConsent: true}, mailer.SendLanes{}, ""},
		{"all there", mailer.ConsentGate{Promotional: true}, mailer.SendLanes{Purpose: true, ListUnsubscribe: true, Lanes: graph}, ""},
		{"no purposes", mailer.ConsentGate{Promotional: true}, mailer.SendLanes{ListUnsubscribe: true, Lanes: graph}, "SKYMAIL_MAIL_PURPOSE"},
		{"no List-Unsubscribe", mailer.ConsentGate{Promotional: true}, mailer.SendLanes{Purpose: true, Lanes: graph}, "SKYMAIL_LIST_UNSUBSCRIBE"},
		{"the duyuru lane off", mailer.ConsentGate{Promotional: true}, mailer.SendLanes{Purpose: true, ListUnsubscribe: true, Lanes: off}, "SKYMAIL_SEND_DUYURU"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.gate.CheckWith(tc.lanes)
			if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("%v, want %q", err, tc.want)
			}
		})
	}
}
