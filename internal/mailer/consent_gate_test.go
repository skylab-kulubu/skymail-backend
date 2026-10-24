package mailer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
	// slow: core takes its time, until the caller gives up.
	slow  bool
	asked []string
}

func (f *fakeConsents) Audience(ctx context.Context, purpose string) ([]consent.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "audience "+purpose)
	if f.slow {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return nil, ctx.Err()
	}
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
	// A row waiting for a fetch is due again at once: the next turn takes it.
	mailer.SetConsentTiming(m, time.Minute, 2*time.Second, 30*time.Second, 0, nil)
	return m
}

// gatedTurn is a turn, the consent fetch it started, and the next turn: the
// rows both took.
func gatedTurn(t *testing.T, m mailer.Transactional) []database.MailQueue {
	t.Helper()
	taken := mailer.Turn(context.Background(), m)
	mailer.WaitConsentFetches(m)
	return append(taken, mailer.Turn(context.Background(), m)...)
}

func duyuruSend(commercialAbsent bool) mailer.SendPurpose {
	purpose, category := mailer.PurposeDuyuru, "club:duyuru"
	return mailer.SendPurpose{Purpose: &purpose, Category: &category, CommercialContentAbsent: &commercialAbsent}
}

func enqueuePurposed(t *testing.T, m mailer.Transactional, purpose mailer.SendPurpose, recipients ...mailer.RecipientInfo) uuid.UUID {
	t.Helper()
	return enqueueTemplate(t, m, initialTemplate, purpose, recipients...)
}

func enqueueTemplate(t *testing.T, m mailer.Transactional, template uuid.UUID, purpose mailer.SendPurpose, recipients ...mailer.RecipientInfo) uuid.UUID {
	t.Helper()
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: template, BodyVariables: []byte(`{"code":"123456"}`), Recipients: recipients, Purpose: purpose,
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

// waitingRow says a row waits: pending, nothing tried, no error, due again
// no sooner than after.
func waitingRow(t *testing.T, db *database.Store, task uuid.UUID, email string, after time.Duration) {
	t.Helper()
	var status, failure string
	var attempts int
	var due bool
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT status::text, coalesce(error, ''), attempts, next_attempt_at >= NOW() + $3 * INTERVAL '1 millisecond'
		FROM mail_queue WHERE task_id = $1 AND lower(recipient_email) = lower($2)`, task, email, int(after/time.Millisecond)).
		Scan(&status, &failure, &attempts, &due); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || failure != "" || attempts != 0 || !due {
		t.Errorf("%s: %s %q, %d attempts, due later %v; want waiting, nothing tried, due no sooner than %s", email, status, failure, attempts, due, after)
	}
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
	if taken := takenTo(gatedTurn(t, m)); len(taken) != 3 {
		t.Fatalf("the sponsor's thanks: taken %d, want all three", len(taken))
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v about a mail that is not promotional", asked)
	}
	if link := storedLink(t, db, thanks, "zeynep@example.com"); link != "" {
		t.Errorf("the thanks got a link %q", link)
	}

	promo := enqueuePurposed(t, m, duyuruSend(false), ayse, baris, zeynep)
	taken := takenTo(gatedTurn(t, m))
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

// The audience is read once and used for consentCacheTTL: a second send in
// that time does not ask core again; after it, core is asked again, and a
// consent withdrawn in between holds the mail.
func TestTheAudienceIsCachedForAMinute(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := promotionalAudience()
	clock := newTestClock()
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, Core: core})
	mailer.SetConsentTiming(m, time.Minute, 2*time.Second, 30*time.Second, 0, clock.now)
	deniz := mailer.RecipientInfo{FullName: "Deniz", Email: "deniz@example.com"}

	enqueuePurposed(t, m, duyuruSend(false), deniz)
	if taken := gatedTurn(t, m); len(taken) != 1 {
		t.Fatalf("taken %d, want Deniz's", len(taken))
	}
	enqueuePurposed(t, m, duyuruSend(false), deniz)
	if taken := mailer.Turn(context.Background(), m); len(taken) != 1 {
		t.Errorf("within the minute: taken %d, want Deniz's from the cache", len(taken))
	}
	if asked := core.calls(); len(asked) != 1 {
		t.Errorf("core asked %v, want once", asked)
	}

	core.mu.Lock()
	core.audiences[consent.PurposePromotional] = core.audiences[consent.PurposePromotional][:1]
	core.mu.Unlock()
	clock.advance(61 * time.Second)
	late := enqueuePurposed(t, m, duyuruSend(false), deniz)
	if taken := gatedTurn(t, m); len(taken) != 0 {
		t.Errorf("after Deniz withdrew: taken %d", len(taken))
	}
	if got := rowTo(t, db, late, "deniz@example.com"); got.error != "suppressed: no_promotional_consent" {
		t.Errorf("Deniz: %+v", got)
	}
}

// Core not answering is not the consent's absence: the mail waits, nothing
// tried and nothing counted; core's breaker keeps it unasked a while, and the
// row backs off. Still unanswered 48 hours after it was queued, it is closed
// undecided (consent: unavailable, not skipped). Without CORE_URL too.
func TestAPromotionalMailWaitsForCore(t *testing.T) {
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
			if taken := gatedTurn(t, m); len(taken) != 0 {
				t.Errorf("taken %d rows", len(taken))
			}
			waitingRow(t, db, promo, "ayse@example.com", 50*time.Second)
			if core, ok := gate.Core.(*fakeConsents); ok && len(core.calls()) != 1 {
				t.Errorf("core asked %v, want once: the breaker is open", core.calls())
			}

			if _, err := db.Conn.Exec(context.Background(),
				`UPDATE mail_queue SET created_at = NOW() - INTERVAL '49 hours', next_attempt_at = NOW() WHERE task_id = $1`, promo); err != nil {
				t.Fatal(err)
			}
			if taken := mailer.Turn(context.Background(), m); len(taken) != 0 {
				t.Errorf("after 48 hours: taken %d rows", len(taken))
			}
			if got := rowTo(t, db, promo, "ayse@example.com"); got.status != "failed" || got.error != "consent: unavailable" || got.attempts != 0 {
				t.Errorf("after 48 hours: %+v, want failed, consent: unavailable", got)
			}
			if status := sendStatus(t, db, promo); status == "sent" {
				t.Errorf("send status %q: an undecided row is not a sent one", status)
			}
		})
	}
}

// A promotional mail sent with a System account template (keycloak.*,
// account.*) is held all the same: the gate reads it before account mail.
func TestAPromotionalMailOnAnAccountTemplateIsHeld(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, Core: promotionalAudience()})
	promo := enqueueTemplate(t, m, accountTemplate, duyuruSend(false), mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"})
	if taken := gatedTurn(t, m); len(taken) != 0 {
		t.Errorf("taken %d rows", len(taken))
	}
	if got := rowTo(t, db, promo, "zeynep@example.com"); got.error != "suppressed: no_promotional_consent" {
		t.Errorf("Zeynep: %+v, want held", got)
	}
}

// A slow core never holds up the turn: the system lane's rows and the bulk
// rows no consent applies to go at once, the promotional row waits, and the
// turn does not wait for core at all.
func TestASlowCoreHoldsUpNothingElse(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := &fakeConsents{slow: true}
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, Core: core})
	mailer.SetConsentTiming(m, time.Minute, 300*time.Millisecond, 30*time.Second, 0, nil)
	promo := enqueuePurposed(t, m, duyuruSend(false), mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com"})
	code := enqueueOne(t, m, accountTemplate, "deniz@example.com")
	thanks := enqueuePurposed(t, m, duyuruSend(true), mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"})

	began := time.Now()
	taken := mailer.Turn(context.Background(), m)
	if took := time.Since(began); took > 200*time.Millisecond {
		t.Errorf("the turn took %s: it waited for core", took)
	}
	got := map[uuid.UUID]bool{}
	for _, row := range taken {
		got[row.TaskID] = true
	}
	if len(taken) != 2 || !got[code] || !got[thanks] {
		t.Errorf("taken %d rows, want the login code and the thanks", len(taken))
	}
	mailer.WaitConsentFetches(m)
	if again := mailer.Turn(context.Background(), m); len(again) != 0 {
		t.Errorf("after core timed out: taken %d rows", len(again))
	}
	waitingRow(t, db, promo, "ayse@example.com", 50*time.Second)
}

// One turn of mixed rows: a promotional mail, a list's duyuru and mail no
// consent applies to; each is decided on its own and the unrelated go first.
func TestAMixedTurn(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := announcementCore()
	core.audiences[consent.PurposePromotional] = promotionalAudience().audiences[consent.PurposePromotional]
	m := gatedMailer(t, db, mailer.ConsentGate{Promotional: true, ListConsent: true, Core: core})
	mailer.PrepareTurns(m, 20) // room for the nine bulk rows in one turn
	list := enqueueToList(t, m, consentList(t, db))
	promo := enqueuePurposed(t, m, duyuruSend(false), mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com"},
		mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"})
	islem, statement := mailer.PurposeIslem, true
	other := enqueuePurposed(t, m, mailer.SendPurpose{Purpose: &islem, TransactionalStatement: &statement}, mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"})

	first := takenTo(mailer.Turn(context.Background(), m))
	if len(first) != 1 || first["misafir@example.com"].TaskID != other {
		t.Errorf("first turn %v, want the islem mail alone", first)
	}
	mailer.WaitConsentFetches(m)
	second := takenTo(mailer.Turn(context.Background(), m))
	if len(second) != 3 || second["Uye@example.com"].TaskID != list || second["rizali@example.com"].TaskID != list || second["ayse@example.com"].TaskID != promo {
		t.Errorf("second turn %v, want the member, the guest who consented and Ayşe", second)
	}
	if got := rowTo(t, db, promo, "zeynep@example.com"); got.error != "suppressed: no_promotional_consent" {
		t.Errorf("Zeynep: %+v", got)
	}
	if got := rowTo(t, db, list, "misafir@example.com"); got.error != "suppressed: no_announcement_consent" {
		t.Errorf("the guest without consent: %+v", got)
	}
}

// consentList is an internal list of a member (an account), a guest who
// consented to announcements on davet-v2, a guest without consent, one
// whose consent was given on davet-v1 (invitations alone), one due the
// renewal question and one whose consent names no text. The names start
// with an ASCII letter: initialTemplate writes the first byte of the name.
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
			       ('Eski', 'eski@example.com'), ('Yenilenecek', 'yenilenecek@example.com'), ('Bos', 'bos@example.com')
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
			{Email: "bos@example.com", TextVersion: "", WithdrawURL: "https://api.example.org/v1/consents/withdraw?token=b"},
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
// Core is asked for the accounts with club:duyuru, whatever the send's
// category (a team removed since cannot fail the question). A Keycloak
// group's members and a mail to one person are not held.
func TestAListAnnouncementToAnAddressWithoutAnAccountNeedsTheConsent(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := announcementCore()
	m := gatedMailer(t, db, mailer.ConsentGate{ListConsent: true, Core: core})
	list := consentList(t, db)
	task := enqueueToList(t, m, list)

	taken := takenTo(gatedTurn(t, m))
	if len(taken) != 2 || taken["Uye@example.com"].ID == uuid.Nil || taken["rizali@example.com"].ID == uuid.Nil {
		t.Fatalf("taken %v, want the member and the guest who consented", taken)
	}
	if taken["rizali@example.com"].ListUnsubscribeUrl != nil {
		t.Errorf("an announcement got the consent's withdraw link")
	}
	for _, email := range []string{"misafir@example.com", "eski@example.com", "yenilenecek@example.com", "bos@example.com"} {
		if got := rowTo(t, db, task, email); got.status != "failed" || got.error != "suppressed: no_announcement_consent" || got.attempts != 0 {
			t.Errorf("%s: %+v, want failed with suppressed: no_announcement_consent", email, got)
		}
	}
	asked := core.calls()
	if len(asked) != 2 || asked[0] != "audience event_invitations" || !strings.HasPrefix(asked[1], "resolve club:duyuru ") {
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
	if taken := mailer.Turn(context.Background(), m); len(taken) != 6 {
		t.Errorf("taken %d, want all six", len(taken))
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v", asked)
	}
}

// Core not answering: no list recipient is dropped. One whose consent core
// still shows goes; the others, the member among them, wait for core.
func TestAListAnnouncementWithoutCoreWaits(t *testing.T) {
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
			taken := takenTo(gatedTurn(t, m))
			waiting := []string{"Uye@example.com", "misafir@example.com", "eski@example.com"}
			if core.resolveDown {
				if len(taken) != 1 || taken["rizali@example.com"].ID == uuid.Nil {
					t.Errorf("taken %v, want the guest whose consent core holds", taken)
				}
			} else {
				if len(taken) != 0 {
					t.Errorf("taken %v, want none", taken)
				}
				waiting = append(waiting, "rizali@example.com")
			}
			for _, email := range waiting {
				waitingRow(t, db, task, email, 0)
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
