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
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// fakeVerdicts is core's recipients:resolve: what each person chose,
// whom each address belongs to, and every question asked.
type fakeVerdicts struct {
	mu sync.Mutex
	// accounts: address → sub; opted: sub → the reason the mail is held.
	accounts map[string]string
	opted    map[string]string
	unknown  map[string]bool
	down     bool
	asked    []string
}

func (f *fakeVerdicts) Resolve(_ context.Context, category string, recipients []consent.Recipient) ([]consent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(recipients))
	for i, r := range recipients {
		names[i] = r.Sub + r.Email
	}
	f.asked = append(f.asked, category+" "+strings.Join(names, ","))
	switch {
	case f.down:
		return nil, errors.New("core answered 503")
	case f.unknown[category]:
		return nil, consent.ErrCategoryUnknown
	}
	results := make([]consent.Result, len(recipients))
	for i, r := range recipients {
		sub := r.Sub
		if sub == "" {
			sub = f.accounts[r.Email]
		}
		if sub == "" {
			continue
		}
		if reason := f.opted[sub]; reason != "" {
			results[i] = consent.Result{Matched: true, Sub: sub, Reason: reason}
			continue
		}
		results[i] = consent.Result{Matched: true, Sub: sub, Allowed: true, UnsubscribeURL: personLink(sub)}
	}
	return results, nil
}

func (f *fakeVerdicts) set(change func(f *fakeVerdicts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeVerdicts) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func personLink(sub string) string {
	return "https://api.example.org/v1/communication-preferences/unsubscribe?t=" + sub
}

var (
	ayseSub  = uuid.MustParse("7d3c1f6e-0000-4000-8000-00000000a1a1")
	barisSub = uuid.MustParse("7d3c1f6e-0000-4000-8000-00000000b2b2")
)

func preferenceCore() *fakeVerdicts {
	return &fakeVerdicts{
		accounts: map[string]string{"uye@example.com": ayseSub.String()},
		opted:    map[string]string{barisSub.String(): consent.ReasonOptedOut},
		unknown:  map[string]bool{},
	}
}

// skymailLinks is SkyMail's own link, on with a key.
func skymailLinks(t *testing.T) unsubscribe.Config {
	t.Helper()
	ring, err := unsubscribe.NewRing(1, map[byte][]byte{1: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	return unsubscribe.Config{On: true, BaseURL: "https://api.example.org/api/skymail", Ring: ring}
}

func preferenceMailer(t *testing.T, db *database.Store, core *fakeVerdicts, on bool) mailer.Transactional {
	t.Helper()
	gate := mailer.PreferenceGate{On: on, Core: core, Links: skymailLinks(t)}
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn,
		mailer.WithSendLanes(mailer.SendLanes{Purpose: true}), mailer.WithPreferenceGate(gate))
	mailer.PrepareTurns(m, 10)
	// A verdict a turn did not have is fetched and used by the next turn.
	mailer.SetPreferenceTiming(m, time.Minute, 2*time.Second, 30*time.Second, 0, nil)
	return m
}

// preferenceTurn is a turn, the fetch it started, and the next turn: the
// rows both took.
func preferenceTurn(t *testing.T, m mailer.Transactional) []database.MailQueue {
	t.Helper()
	taken := mailer.Turn(context.Background(), m)
	mailer.WaitPreferenceFetches(m)
	return append(taken, mailer.Turn(context.Background(), m)...)
}

func checkedAt(t *testing.T, db *database.Store, task uuid.UUID, email string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := db.Conn.QueryRow(context.Background(),
		`SELECT preference_checked_at FROM mail_queue WHERE task_id = $1 AND lower(recipient_email) = lower($2)`, task, email).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func islemSend() mailer.SendPurpose {
	purpose, statement := mailer.PurposeIslem, true
	return mailer.SendPurpose{Purpose: &purpose, TransactionalStatement: &statement}
}

// While the check is on, a duyuru send is asked about before it is queued:
// a member known by sub who allows it gets core's link and the time it was
// checked; one who turned the category off is written failed, nothing
// tried, preference: opted_out (skipped: the send does not read failed); an
// address core matches to an account is asked by address and gets that
// person's link; one core matches to none gets SkyMail's own link; an
// address that left through SkyMail's link stays out. One question for the
// whole send, by sub where the send knows it.
func TestADuyuruSendObeysCoreWhenItIsQueued(t *testing.T) {
	logs := captureMailerLogs(t)
	db := unsubscribeQueue(t)
	core := preferenceCore()
	m := preferenceMailer(t, db, core, true)

	task := enqueuePurposed(t, m, duyuruSend(true),
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayseSub},
		mailer.RecipientInfo{FullName: "Barış", Email: "baris@example.com", Sub: &barisSub},
		mailer.RecipientInfo{FullName: "Uye", Email: "Uye@Example.com"},
		mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"},
		mailer.RecipientInfo{FullName: "Zeynep", Email: "zeynep@example.com"},
	)
	asked := core.calls()
	if len(asked) != 1 || asked[0] != "club:duyuru "+ayseSub.String()+","+barisSub.String()+",uye@example.com,misafir@example.com,zeynep@example.com" {
		t.Fatalf("core asked %v, want one question by sub and by address", asked)
	}
	if link := storedLink(t, db, task, "ayse@example.com"); link != personLink(ayseSub.String()) || checkedAt(t, db, task, "ayse@example.com") == nil {
		t.Errorf("Ayşe: link %q, checked %v; want core's link, checked", link, checkedAt(t, db, task, "ayse@example.com"))
	}
	if got := rowTo(t, db, task, "baris@example.com"); got.status != "failed" || got.error != "preference: opted_out" || got.attempts != 0 {
		t.Errorf("Barış: %+v, want failed with preference: opted_out and nothing tried", got)
	}
	if link := storedLink(t, db, task, "Uye@Example.com"); link != personLink(ayseSub.String()) {
		t.Errorf("the account's address: link %q, want its person's", link)
	}
	if link := storedLink(t, db, task, "misafir@example.com"); !strings.HasPrefix(link, "https://api.example.org/api/skymail/u/") || checkedAt(t, db, task, "misafir@example.com") != nil {
		t.Errorf("the address without an account: link %q, want SkyMail's own, no verdict", link)
	}
	if got := rowTo(t, db, task, "zeynep@example.com"); got.status != "failed" || got.error != "suppressed: unsubscribed" {
		t.Errorf("Zeynep: %+v, want her unsubscribe record", got)
	}
	var status string
	if err := db.Conn.QueryRow(context.Background(), `SELECT mail_task_status($1)`, task).Scan(&status); err != nil || status != "sending" {
		t.Errorf("send status %q %v, want sending: the held are skipped", status, err)
	}
	logs.mu.Lock()
	written := logs.buf.String()
	logs.mu.Unlock()
	for _, secret := range []string{"baris@example.com", "misafir@example.com", ayseSub.String()} {
		if strings.Contains(written, secret) {
			t.Errorf("the log names %s", secret)
		}
	}
	// The verdicts of the queueing hold for the first turn: core is not
	// asked again, and every row taken carries its link.
	taken := takenTo(mailer.Turn(context.Background(), m))
	if len(taken) != 3 || len(core.calls()) != 1 {
		t.Fatalf("taken %d rows, core asked %v; want three, no new question", len(taken), core.calls())
	}
}

// Core not answering, or calling the team no team, writes nothing.
func TestADuyuruSendCoreCannotAnswerForWritesNothing(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	core.down = true
	m := preferenceMailer(t, db, core, true)
	_, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Purpose: duyuruSend(true),
		Recipients: []mailer.RecipientInfo{{Email: "ayse@example.com", Sub: &ayseSub}},
	})
	if !errors.Is(err, mailer.ErrPreferenceUnavailable) {
		t.Fatalf("core down: %v, want %v", err, mailer.ErrPreferenceUnavailable)
	}
	_, err = m.EnqueueTrackedSingle(context.Background(), database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &initialTemplate, BodyVariables: []byte(`{}`), RecipientFullName: "Ayşe", RecipientEmail: "ayse@example.com",
		Purpose: duyuruSend(true).Purpose, Category: duyuruSend(true).Category,
	}, nil)
	if !errors.Is(err, mailer.ErrPreferenceUnavailable) {
		t.Fatalf("one person, core down: %v", err)
	}
	core.set(func(f *fakeVerdicts) {
		f.down, f.unknown["team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru"] = false, true
	})
	purpose, category := mailer.PurposeDuyuru, "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:duyuru"
	_, err = m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Purpose: mailer.SendPurpose{Purpose: &purpose, Category: &category},
		Recipients: []mailer.RecipientInfo{{Email: "ayse@example.com", Sub: &ayseSub}},
	})
	if !errors.Is(err, mailer.ErrPreferenceCategoryUnknown) {
		t.Fatalf("no team: %v, want %v", err, mailer.ErrPreferenceCategoryUnknown)
	}
	var tasks, rows int
	if err := db.Conn.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM mail_tasks), (SELECT count(*) FROM mail_queue)`).Scan(&tasks, &rows); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 || rows != 0 {
		t.Errorf("%d sends and %d rows written, want none", tasks, rows)
	}
}

// Every turn decides again about the rows core gave a verdict about, right
// before sending, from what core said within the cache's time: a person who
// turned the category off after the send was queued gets none of it; one
// question per turn's category; while core does not answer such rows wait,
// and an address without an account does not.
func TestEveryTurnAsksCoreAgainBeforeSending(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	clock := newTestClock()
	m := preferenceMailer(t, db, core, true)
	mailer.SetPreferenceTiming(m, time.Minute, 2*time.Second, 30*time.Second, 0, clock.now)

	cemSub := uuid.MustParse("7d3c1f6e-0000-4000-8000-00000000c3c3")
	task := enqueuePurposed(t, m, duyuruSend(true),
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayseSub},
		mailer.RecipientInfo{FullName: "Cem", Email: "cem@example.com", Sub: &cemSub},
		mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"},
	)
	// After it was queued Cem turns the category off, and the verdicts age.
	core.set(func(f *fakeVerdicts) { f.opted[cemSub.String()] = consent.ReasonOptedOut })
	clock.advance(2 * time.Minute)

	first := takenTo(mailer.Turn(context.Background(), m))
	if len(first) != 1 || first["misafir@example.com"].ID == uuid.Nil {
		t.Fatalf("first turn took %v, want the address without an account alone", first)
	}
	for _, email := range []string{"ayse@example.com", "cem@example.com"} {
		if got := rowTo(t, db, task, email); got.status != "pending" || got.error != "" || got.attempts != 0 {
			t.Errorf("%s: %+v, want put back, nothing tried", email, got)
		}
	}
	mailer.WaitPreferenceFetches(m)
	if asked := core.calls(); len(asked) != 2 || !strings.HasPrefix(asked[1], "club:duyuru ") {
		t.Fatalf("core asked %v, want the queueing's question and one for the turn", asked)
	}
	second := takenTo(mailer.Turn(context.Background(), m))
	if len(second) != 1 || second["ayse@example.com"].ID == uuid.Nil {
		t.Fatalf("second turn took %v, want Ayşe", second)
	}
	if got := rowTo(t, db, task, "cem@example.com"); got.status != "failed" || got.error != "preference: opted_out" || got.attempts != 0 {
		t.Errorf("Cem: %+v, want held before his mail went out", got)
	}
	if at := checkedAt(t, db, task, "ayse@example.com"); at == nil || !at.After(clock.now().Add(-time.Second)) {
		t.Errorf("Ayşe checked at %v, want the turn's verdict", at)
	}

	// Core down: the rows core decides about wait; the others go.
	core.set(func(f *fakeVerdicts) { f.down = true })
	again := enqueueAfterCore(t, m, core, clock)
	taken := takenTo(preferenceTurn(t, m))
	if len(taken) != 1 || taken["misafir2@example.com"].ID == uuid.Nil {
		t.Fatalf("core down: taken %v, want the address without an account alone", taken)
	}
	// Core's breaker is open: the row waits a while.
	waitingRow(t, db, again, "ayse@example.com", 50*time.Second)
}

// enqueueAfterCore queues Ayşe and an address without an account while core
// answers, then lets the verdicts age.
func enqueueAfterCore(t *testing.T, m mailer.Transactional, core *fakeVerdicts, clock *testClock) uuid.UUID {
	t.Helper()
	core.set(func(f *fakeVerdicts) { f.down = false })
	task := enqueuePurposed(t, m, duyuruSend(true),
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayseSub},
		mailer.RecipientInfo{FullName: "Misafir", Email: "misafir2@example.com"},
	)
	core.set(func(f *fakeVerdicts) { f.down = true })
	clock.advance(2 * time.Minute)
	return task
}

// The safety net: while the check is on a duyuru row without a link never
// goes out (a send queued without core's verdicts, a row from before the
// switch); an islem send is neither asked about nor held.
func TestADuyuruRowWithoutALinkIsNeverSent(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	off := preferenceMailer(t, db, core, false)
	before := enqueuePurposed(t, off, duyuruSend(true), mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"})
	transactional := enqueuePurposed(t, off, islemSend(), mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"})

	on := preferenceMailer(t, db, core, true)
	taken := mailer.Turn(context.Background(), on)
	if len(taken) != 1 || taken[0].TaskID != transactional {
		t.Fatalf("taken %+v, want the islem row alone", taken)
	}
	if got := rowTo(t, db, before, "misafir@example.com"); got.status != "failed" || got.error != "unsubscribe_missing" || got.attempts != 0 {
		t.Errorf("the duyuru row without a link: %+v, want failed unsubscribe_missing, nothing tried", got)
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v", asked)
	}
	enqueuePurposed(t, on, islemSend(), mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"})
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("an islem send asked core %v", asked)
	}
}

// Off, nothing of the check runs: core is never asked, and a duyuru row
// without a link goes out as before.
func TestTheCheckOffSendsAsBefore(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	m := preferenceMailer(t, db, core, false)
	task := enqueuePurposed(t, m, duyuruSend(true),
		mailer.RecipientInfo{FullName: "Barış", Email: "baris@example.com", Sub: &barisSub},
		mailer.RecipientInfo{FullName: "Misafir", Email: "misafir@example.com"},
	)
	if taken := mailer.Turn(context.Background(), m); len(taken) != 2 {
		t.Fatalf("taken %d, want both", len(taken))
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v", asked)
	}
	if link := storedLink(t, db, task, "misafir@example.com"); link != "" {
		t.Errorf("a link %q while the check is off", link)
	}
}

// A team deleted after the send was queued: core calls its category no
// team's; nobody can say, so the rows are written failed, not skipped.
func TestATeamGoneSinceQueueingHoldsItsRows(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	clock := newTestClock()
	m := preferenceMailer(t, db, core, true)
	mailer.SetPreferenceTiming(m, time.Minute, 2*time.Second, 30*time.Second, 0, clock.now)
	purpose, category := mailer.PurposeDuyuru, "team:6f1d2c3b-0a9e-4f8d-8c7b-6a5e4d3c2b1a:haber"
	absent := true
	task := enqueuePurposed(t, m, mailer.SendPurpose{Purpose: &purpose, Category: &category, CommercialContentAbsent: &absent},
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayseSub})
	core.set(func(f *fakeVerdicts) { f.unknown[category] = true })
	clock.advance(2 * time.Minute)
	if taken := preferenceTurn(t, m); len(taken) != 0 {
		t.Fatalf("taken %d", len(taken))
	}
	mailer.Turn(context.Background(), m)
	if got := rowTo(t, db, task, "ayse@example.com"); got.status != "failed" || got.error != "preference: category_unknown" {
		t.Errorf("Ayşe: %+v, want failed preference: category_unknown", got)
	}
}

// SKYMAIL_BULK_PREFERENCE reads off or on; on needs purposes, the one-click
// header on a duyuru lane that is not off, and SkyMail's own link with a
// key.
func TestPreferenceGateConfiguration(t *testing.T) {
	for value, want := range map[string]bool{"": false, "off": false, "ON": true} {
		gate, err := mailer.PreferenceGateFromEnv(getenv(map[string]string{mailer.PreferenceEnv: value}))
		if err != nil || gate.On != want {
			t.Errorf("%q: %+v %v", value, gate, err)
		}
	}
	if _, err := mailer.PreferenceGateFromEnv(getenv(map[string]string{mailer.PreferenceEnv: "yes"})); err == nil {
		t.Error("yes accepted")
	}
	graph := map[mailer.Lane]mailer.LaneConfig{mailer.LaneDuyuru: {Transport: mailer.TransportGraph}}
	all := mailer.SendLanes{Purpose: true, ListUnsubscribe: true, Lanes: graph}
	links := skymailLinks(t)
	for name, tc := range map[string]struct {
		gate  mailer.PreferenceGate
		lanes mailer.SendLanes
		want  string
	}{
		"off needs nothing":     {mailer.PreferenceGate{}, mailer.SendLanes{}, ""},
		"all there":             {mailer.PreferenceGate{On: true, Links: links}, all, ""},
		"no purposes":           {mailer.PreferenceGate{On: true, Links: links}, mailer.SendLanes{ListUnsubscribe: true, Lanes: graph}, "SKYMAIL_MAIL_PURPOSE"},
		"no List-Unsubscribe":   {mailer.PreferenceGate{On: true, Links: links}, mailer.SendLanes{Purpose: true, Lanes: graph}, "SKYMAIL_LIST_UNSUBSCRIBE"},
		"the duyuru lane off":   {mailer.PreferenceGate{On: true, Links: links}, mailer.SendLanes{Purpose: true, ListUnsubscribe: true}, "SKYMAIL_SEND_DUYURU"},
		"SkyMail's link off":    {mailer.PreferenceGate{On: true}, all, "SKYMAIL_UNSUBSCRIBE_LINK"},
		"SkyMail's link no key": {mailer.PreferenceGate{On: true, Links: unsubscribe.Config{On: true, BaseURL: links.BaseURL, Ring: &unsubscribe.Ring{}}}, all, "SKYMAIL_UNSUBSCRIBE_KEY_V"},
	} {
		err := tc.gate.CheckWith(tc.lanes)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want it to name %s", name, err, tc.want)
		}
	}
}

// A row that brought core's link and knows its person (a news send's, from
// core's audience) is not asked about when it is queued, keeps its link, and
// is decided in its turn like any other: a person who turned the category
// off since gets none of it.
func TestARowThatKnowsItsPersonIsDecidedInItsTurn(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	m := preferenceMailer(t, db, core, true)
	task := enqueuePurposed(t, m, duyuruSend(true),
		mailer.RecipientInfo{FullName: "Barış", Email: "baris@example.com", Sub: &barisSub, UnsubscribeURL: personLink(barisSub.String())},
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.com", Sub: &ayseSub, UnsubscribeURL: personLink(ayseSub.String())},
	)
	if asked := core.calls(); len(asked) != 0 {
		t.Fatalf("core asked %v when the send was queued", asked)
	}
	if link := storedLink(t, db, task, "baris@example.com"); link != personLink(barisSub.String()) {
		t.Fatalf("Barış's link %q, want the one he brought", link)
	}
	taken := takenTo(preferenceTurn(t, m))
	if len(taken) != 1 || taken["ayse@example.com"].ID == uuid.Nil {
		t.Fatalf("taken %v, want Ayşe alone", taken)
	}
	if got := rowTo(t, db, task, "baris@example.com"); got.status != "failed" || got.error != "preference: opted_out" {
		t.Errorf("Barış: %+v, want held in his turn", got)
	}
}

// A test mail to its own sender's verified address alone (self_test, a News
// send's test mail, which carries no List-Unsubscribe) is neither asked
// about when it is queued nor held in its turn for a missing link; another
// duyuru send without a link still is.
func TestATestMailToItsSenderIsLeftBe(t *testing.T) {
	db := suppressionQueue(t)
	core := preferenceCore()
	m := preferenceMailer(t, db, core, true)
	test, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), Purpose: duyuruSend(true), SelfTest: true,
		Recipients: []mailer.RecipientInfo{{FullName: "Ayse", Email: "ayse@example.com", Sub: &ayseSub}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Fatalf("core asked %v about a test mail", asked)
	}
	if link := storedLink(t, db, test, "ayse@example.com"); link != "" {
		t.Fatalf("the test mail got a link %q", link)
	}
	taken := preferenceTurn(t, m)
	if len(taken) != 1 || taken[0].TaskID != test {
		t.Fatalf("taken %+v, want the test mail", taken)
	}
	if asked := core.calls(); len(asked) != 0 {
		t.Errorf("core asked %v in the test mail's turn", asked)
	}
	var selfTest bool
	if err := db.Conn.QueryRow(context.Background(), `SELECT self_test FROM mail_tasks WHERE id = $1`, test).Scan(&selfTest); err != nil || !selfTest {
		t.Errorf("self_test %v %v, want true", selfTest, err)
	}
}
