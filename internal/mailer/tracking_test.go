package mailer_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

const trackingBase = "https://api.yildizskylab.com/api/skymail"

var (
	announcementTemplate = uuid.MustParse("70000000-0000-4000-8000-000000000001")
	systemTemplate       = uuid.MustParse("70000000-0000-4000-8000-000000000002")
	sensitiveTemplate    = uuid.MustParse("70000000-0000-4000-8000-000000000003")
	lockedKeyTemplate    = uuid.MustParse("70000000-0000-4000-8000-000000000004")
	announcementList     = uuid.MustParse("70000000-0000-4000-8000-000000000005")

	ayse   = uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10")
	mehmet = uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a11")
	zeynep = uuid.MustParse("7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a12")
)

// The announcement every tracking test sends: a link every recipient gets
// (twice), one a variable makes personal, a mail address, an unsubscribe
// link and one the template keeps from tracking.
const announcementHTML = `<!DOCTYPE html><html><head><style>a{color:#003694}</style></head><body style="margin:0">
<p>Merhaba {{.FullName}},</p>
<a href="https://skylab.example/etkinlik?gun=1&amp;saat=2">Etkinlik</a>
<a href="https://skylab.example/form?e={{.Email}}">Sana özel</a>
<a href="mailto:yk@example.com">Yaz</a>
<a href="https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=abc">Çık</a>
<a href="https://skylab.example/gizli" data-skymail-notrack>Sayılmaz</a>
<a href="https://skylab.example/etkinlik?gun=1&amp;saat=2">Yine etkinlik</a>
</body></html>`

// trackingQueue is a migrated database with an announcement, a System
// template, a sensitive one, one whose key is locked sensitive, and a
// mailing list of Ayşe and Mehmet by address.
func trackingQueue(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, system, content_sensitive)
		VALUES ($1, 'club.duyuru', 'Duyuru', 'Duyuru', $5, 'Merhaba {{.FullName}}: https://skylab.example/etkinlik', '', false, false),
		       ($2, 'ops.bildirim', 'Sistem', 'Sistem', $5, 'x', '', true, false),
		       ($3, 'club.hassas', 'Hassas', 'Hassas', $5, 'x', '', false, true),
		       ($4, 'event.ticket-created', 'Bilet', 'Bilet', $5, 'x', '', false, NULL)`,
		announcementTemplate, systemTemplate, sensitiveTemplate, lockedKeyTemplate, announcementHTML); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO mailing_lists (id, name) VALUES ($1, 'Duyuru');
		`, announcementList); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		WITH people AS (
			INSERT INTO recipients (full_name, email)
			VALUES ('Ayşe Yılmaz', 'ayse@example.com'), ('Mehmet Kaya', 'mehmet@example.com')
			RETURNING id)
		INSERT INTO mailing_list_recipients (mail_list_id, recipient_id) SELECT $1, id FROM people`,
		announcementList); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

// fakePreferences is core's answer: who may be tracked, and every call made.
// With db, each call also counts the database's sessions that sit in an open
// transaction while core is asked: a send must ask core before its
// transaction begins, never while it holds one.
type fakePreferences struct {
	mu        sync.Mutex
	decisions map[uuid.UUID]tracking.Decision
	err       error
	calls     [][]uuid.UUID
	db        *database.Store
	openTx    []int
}

func (p *fakePreferences) Resolve(ctx context.Context, subs []uuid.UUID) (map[uuid.UUID]tracking.Decision, error) {
	open := 0
	if p.db != nil {
		if err := p.db.Conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]uuid.UUID(nil), subs...))
	p.openTx = append(p.openTx, open)
	if p.err != nil {
		return nil, p.err
	}
	out := map[uuid.UUID]tracking.Decision{}
	for _, sub := range subs {
		out[sub] = p.decisions[sub]
	}
	return out, nil
}

func (p *fakePreferences) asked() [][]uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]uuid.UUID(nil), p.calls...)
}

// coreSays: Ayşe may be tracked, Mehmet turned it off, Zeynep is not a
// Member.
func coreSays() *fakePreferences {
	return &fakePreferences{decisions: map[uuid.UUID]tracking.Decision{
		ayse:   {Allowed: true},
		mehmet: {Allowed: false, Reason: tracking.SkipPreferenceOff},
		zeynep: {Allowed: false, Reason: tracking.SkipNotMember},
	}}
}

func trackingOn(prefs tracking.Preferences) mailer.Option {
	return mailer.WithTracking(mailer.Tracking{
		Config:      tracking.Config{Mode: tracking.ModeOn, BaseURL: trackingBase, CoreURL: "http://core.invalid"},
		Preferences: prefs,
	})
}

// groupOf is a Keycloak group of Ayşe, Mehmet and Zeynep, and a member
// whose id SkyMail does not know.
func groupOf() []mailer.RecipientInfo {
	return []mailer.RecipientInfo{
		{FullName: "Ayşe Yılmaz", Email: "ayse@example.com", Sub: &ayse},
		{FullName: "Mehmet Kaya", Email: "mehmet@example.com", Sub: &mehmet},
		{FullName: "Zeynep Ak", Email: "zeynep@example.com", Sub: &zeynep},
		{FullName: "Adı Bilinen", Email: "adres@example.com"},
	}
}

type trackedRow struct {
	id         uuid.UUID
	token      []byte
	reason     string
	disabledAt *time.Time
	sentAt     *time.Time
	bodyHTML   string
	body       string
	status     string
}

func trackedRows(t *testing.T, db *database.Store, task uuid.UUID) map[string]trackedRow {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(), `
		SELECT recipient_email, id, tracking_token, coalesce(tracking_skip_reason, ''), tracking_disabled_at, sent_at,
		       coalesce(body_html, ''), body, status::text
		FROM mail_queue WHERE task_id = $1`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]trackedRow{}
	for rows.Next() {
		var email string
		var r trackedRow
		if err := rows.Scan(&email, &r.id, &r.token, &r.reason, &r.disabledAt, &r.sentAt, &r.bodyHTML, &r.body, &r.status); err != nil {
			t.Fatal(err)
		}
		out[email] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func trackingRequested(t *testing.T, db *database.Store, task uuid.UUID) bool {
	t.Helper()
	var requested bool
	if err := db.Conn.QueryRow(context.Background(), `SELECT tracking_requested FROM mail_tasks WHERE id = $1`, task).Scan(&requested); err != nil {
		t.Fatal(err)
	}
	return requested
}

type registeredLink struct {
	url   string
	token []byte
}

func linksOf(t *testing.T, db *database.Store, task uuid.UUID) []registeredLink {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(), `
		SELECT url, link_token FROM mail_task_links WHERE task_id = $1 ORDER BY position`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var links []registeredLink
	for rows.Next() {
		var l registeredLink
		if err := rows.Scan(&l.url, &l.token); err != nil {
			t.Fatal(err)
		}
		links = append(links, l)
	}
	return links
}

// A tracked send to a Keycloak group: each person core lets be tracked gets
// a token of their own; everyone else gets why not; core is asked once about
// the people with an id, and only them. The links every recipient has are
// registered with a token each; a personal one, a mail address, an
// unsubscribe link and a link marked data-skymail-notrack are not.
func TestATrackedGroupSendGivesATokenToWhomCoreAllows(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))
	if !m.TrackingAvailable() {
		t.Fatal("TrackingAvailable() = false with SKYMAIL_TRACKING=on")
	}

	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trackingRequested(t, db, task) {
		t.Error("the send does not say it asked for tracking")
	}
	rows := trackedRows(t, db, task)
	if got := rows["ayse@example.com"]; len(got.token) != tracking.RecipientTokenBytes || got.reason != "" {
		t.Errorf("Ayşe: token %x, reason %q; want a token of her own", got.token, got.reason)
	}
	for email, reason := range map[string]string{
		"mehmet@example.com": tracking.SkipPreferenceOff,
		"zeynep@example.com": tracking.SkipNotMember,
		"adres@example.com":  tracking.SkipNoAccount,
	} {
		if got := rows[email]; got.token != nil || got.reason != reason {
			t.Errorf("%s: token %x, reason %q; want none, %q", email, got.token, got.reason, reason)
		}
	}

	asked := prefs.asked()
	if len(asked) != 1 {
		t.Fatalf("core was asked %d times, want once", len(asked))
	}
	slices.SortFunc(asked[0], func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(asked[0], []uuid.UUID{ayse, mehmet, zeynep}) {
		t.Errorf("core was asked about %v, want the three with an id", asked[0])
	}

	links := linksOf(t, db, task)
	if len(links) != 1 || links[0].url != "https://skylab.example/etkinlik?gun=1&saat=2" || len(links[0].token) != tracking.LinkTokenBytes {
		t.Fatalf("registered links = %+v, want the one every recipient has", links)
	}
}

// Core is asked before the send's transaction begins: no transaction is
// open, and so no lock or connection is held, while SkyMail waits for core
// (up to 5 seconds).
func TestCoreIsAskedBeforeTheSendsTransaction(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	prefs.db = db
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, MailListID: &announcementList, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs.openTx) != 1 || prefs.openTx[0] != 0 {
		t.Fatalf("core was asked %d times with %v transactions open; want once, with none", len(prefs.openTx), prefs.openTx)
	}
	if rows := trackedRows(t, db, task); len(rows["ayse@example.com"].token) != tracking.RecipientTokenBytes {
		t.Fatalf("Ayşe is not tracked: %+v", rows)
	}
}

// A send queued in a caller's transaction (a Mail onayı approval) never asks
// core in it: it takes what the caller asked core before the transaction
// (ResolveTracking). Whoever is not in that answer — a member who joined the
// group since, or no answer at all — is not tracked (preference_unavailable),
// and the send goes out.
func TestASendInATransactionTracksOnlyWhomCoreWasAskedAbout(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	prefs.db = db
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))
	ctx := context.Background()
	group := groupOf()

	// Asked about Ayşe and the member without an id only, before the
	// transaction; Mehmet and Zeynep are in the send but not in the answer.
	asked := m.ResolveTracking(ctx, announcementTemplate, &announcementList, []mailer.RecipientInfo{group[0], group[3]})
	if len(prefs.openTx) != 1 || prefs.openTx[0] != 0 {
		t.Fatalf("ResolveTracking asked core %d times with %v transactions open", len(prefs.openTx), prefs.openTx)
	}
	queueIn := func(preferences *mailer.TrackingPreferences) uuid.UUID {
		t.Helper()
		var task uuid.UUID
		if err := db.InTx(ctx, func(q *database.Queries) error {
			var err error
			task, err = m.Queue(q, mailer.PriorityBulk).EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
				SentBy: laneSender, TemplateID: announcementTemplate, MailListID: &announcementList, BodyVariables: []byte(`{}`),
				Recipients: group, Tracking: true, Preferences: preferences,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return task
	}

	rows := trackedRows(t, db, queueIn(asked))
	want := map[string]string{
		"mehmet@example.com": tracking.SkipPreferenceUnavailable,
		"zeynep@example.com": tracking.SkipPreferenceUnavailable,
		"adres@example.com":  tracking.SkipNoAccount,
	}
	if len(rows["ayse@example.com"].token) != tracking.RecipientTokenBytes {
		t.Errorf("Ayşe, asked about and allowed: %+v", rows["ayse@example.com"])
	}
	for email, reason := range want {
		if rows[email].token != nil || rows[email].reason != reason {
			t.Errorf("%s: token %x, reason %q; want none, %q", email, rows[email].token, rows[email].reason, reason)
		}
	}

	rows = trackedRows(t, db, queueIn(nil))
	for email, row := range rows {
		reason := tracking.SkipPreferenceUnavailable
		if email == "adres@example.com" {
			reason = tracking.SkipNoAccount
		}
		if row.token != nil || row.reason != reason || row.status != "pending" {
			t.Errorf("not asked, %s: %s, token %x, reason %q; want pending, none, %q", email, row.status, row.token, row.reason, reason)
		}
	}
	if len(prefs.asked()) != 1 {
		t.Errorf("core was asked %d times; a send in a transaction never asks it", len(prefs.asked()))
	}
}

// Single mail (the system lane), a System template, a sensitive template and
// a template whose key is locked sensitive are never tracked, the flag on and
// tracking asked for: no token, no link, core not asked, and the send says it
// is not tracked (tracking_requested false, no row reasons), as
// TrackingApplies told the caller beforehand.
func TestNoTokenWhereTrackingNeverGoes(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))
	ctx := context.Background()

	for template, applies := range map[uuid.UUID]bool{
		announcementTemplate: true, systemTemplate: false, sensitiveTemplate: false, lockedKeyTemplate: false,
	} {
		if got := m.TrackingApplies(ctx, template, &announcementList); got != applies {
			t.Errorf("TrackingApplies(%s) = %v, want %v", template, got, applies)
		}
	}
	if m.TrackingApplies(ctx, uuid.New(), &announcementList) {
		t.Error("TrackingApplies of a template that is not there")
	}

	var tasks []uuid.UUID
	single := announcementTemplate
	task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &single, BodyVariables: []byte(`{}`),
		RecipientFullName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.com", TrackingRequested: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks = append(tasks, task)
	for _, template := range []uuid.UUID{systemTemplate, sensitiveTemplate, lockedKeyTemplate} {
		task, err := m.EnqueueWithRecipients(ctx, mailer.EnqueueWithRecipientsParams{
			SentBy: laneSender, TemplateID: template, BodyVariables: []byte(`{}`),
			Recipients: groupOf(), Tracking: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}

	for i, task := range tasks {
		if trackingRequested(t, db, task) {
			t.Errorf("send %d says it is tracked", i)
		}
		for email, row := range trackedRows(t, db, task) {
			if row.token != nil || row.reason != "" {
				t.Errorf("send %d, %s: token %x, reason %q; want neither", i, email, row.token, row.reason)
			}
		}
		if links := linksOf(t, db, task); len(links) != 0 {
			t.Errorf("send %d registered links %+v", i, links)
		}
	}
	if asked := prefs.asked(); len(asked) != 0 {
		t.Errorf("core was asked %v; a send never tracked asks no one", asked)
	}
}

// A core that cannot say is no one tracked in that send, and the send goes
// on: every row is queued, each saying preference_unavailable.
func TestASendCoreCannotAnswerForTracksNoOneAndGoesOut(t *testing.T) {
	logs := captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := &fakePreferences{err: errors.New("core answered 503")}
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))

	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatalf("the send failed with core down: %v", err)
	}
	rows := trackedRows(t, db, task)
	if len(rows) != 4 {
		t.Fatalf("%d rows queued, want all 4", len(rows))
	}
	for email, row := range rows {
		want := tracking.SkipPreferenceUnavailable
		if email == "adres@example.com" {
			want = tracking.SkipNoAccount
		}
		if row.status != "pending" || row.token != nil || row.reason != want {
			t.Errorf("%s: %s, token %x, reason %q; want pending, none, %s", email, row.status, row.token, row.reason, want)
		}
	}
	if links := linksOf(t, db, task); len(links) != 0 {
		t.Errorf("registered links %+v with no one tracked", links)
	}
	found := false
	for _, line := range logs.lines(t) {
		if line["event"] == "tracking_preferences_unavailable" {
			found = true
			for _, sub := range []uuid.UUID{ayse, mehmet, zeynep} {
				for _, value := range line {
					if s, ok := value.(string); ok && strings.Contains(s, sub.String()) {
						t.Errorf("the log line names a person: %v", line)
					}
				}
			}
		}
	}
	if !found {
		t.Error(`no "event":"tracking_preferences_unavailable" line`)
	}
}

// slowCore is a core that does not answer: it reports the time the send gave
// it and gives up when that runs out.
type slowCore struct {
	mu       sync.Mutex
	budget   time.Duration
	deadline bool
}

func (c *slowCore) Resolve(ctx context.Context, _ []uuid.UUID) (map[uuid.UUID]tracking.Decision, error) {
	deadline, ok := ctx.Deadline()
	c.mu.Lock()
	c.deadline, c.budget = ok, time.Until(deadline)
	c.mu.Unlock()
	return nil, context.DeadlineExceeded
}

// A send gives core 5 seconds and no more: past them no one in the send is
// tracked and it goes out.
func TestASendGivesCoreFiveSeconds(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	core := &slowCore{}
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(core))
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, MailListID: &announcementList, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !core.deadline || core.budget <= 4*time.Second || core.budget > 5*time.Second {
		t.Errorf("core was given %s (deadline %v), want 5 seconds", core.budget, core.deadline)
	}
	for email, row := range trackedRows(t, db, task) {
		if row.token != nil || row.status != "pending" {
			t.Errorf("%s: %s, token %x; want pending and not tracked", email, row.status, row.token)
		}
	}
}

// With SKYMAIL_TRACKING off, tracking asked for is ignored: the send says it
// did not ask, no row has a token or a reason, nothing is registered, core
// is not asked.
func TestTrackingOffIgnoresTheRequest(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, mailer.WithTracking(mailer.Tracking{
		Config: tracking.Config{Mode: tracking.ModeOff}, Preferences: prefs,
	}))
	if m.TrackingAvailable() {
		t.Fatal("TrackingAvailable() = true with SKYMAIL_TRACKING=off")
	}
	template, list := announcementTemplate, announcementList
	group, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf(), Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := m.Enqueue(context.Background(), database.CreateMailTaskParams{
		SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`), TrackingRequested: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []uuid.UUID{group, listed} {
		if trackingRequested(t, db, task) {
			t.Error("the send says it asked for tracking with the flag off")
		}
		for email, row := range trackedRows(t, db, task) {
			if row.token != nil || row.reason != "" {
				t.Errorf("%s: token %x, reason %q; want neither", email, row.token, row.reason)
			}
		}
		if links := linksOf(t, db, task); len(links) != 0 {
			t.Errorf("registered links %+v", links)
		}
	}
	if asked := prefs.asked(); len(asked) != 0 {
		t.Errorf("core was asked %v with the flag off", asked)
	}
	// No option at all is off too.
	if mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn).TrackingAvailable() {
		t.Error("a mailer made without tracking has it available")
	}
}

// A mailing list's people are known by address only: none is tracked and
// core is not asked.
func TestAListSendTracksNoOne(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	prefs := coreSays()
	m := mailer.NewMailer(db, startFakeSMTP(t).config(), mailer.SenderOn, trackingOn(prefs))
	template, list := announcementTemplate, announcementList
	task, err := m.Enqueue(context.Background(), database.CreateMailTaskParams{
		SentBy: laneSender, TemplateID: &template, MailListID: &list, BodyVariables: []byte(`{}`), TrackingRequested: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !trackingRequested(t, db, task) {
		t.Error("the list send does not say it asked for tracking")
	}
	rows := trackedRows(t, db, task)
	if len(rows) != 2 {
		t.Fatalf("rows %v", rows)
	}
	for email, row := range rows {
		if row.token != nil || row.reason != tracking.SkipNoAccount {
			t.Errorf("%s: token %x, reason %q; want none, no_account", email, row.token, row.reason)
		}
	}
	if asked := prefs.asked(); len(asked) != 0 {
		t.Errorf("core was asked %v about a list", asked)
	}
}

// relayedMail is one mail as the relay took it: its recipient, its HTML and
// its plain text, decoded.
type relayedMail struct {
	to, html, text string
}

func parseRelayed(t *testing.T, raw string) relayedMail {
	t.Helper()
	// The relay keeps DATA as sent: undo the dot-stuffing.
	var unstuffed strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		unstuffed.WriteString(line + "\r\n")
	}
	msg, err := mail.ReadMessage(strings.NewReader(unstuffed.String()))
	if err != nil {
		t.Fatal(err)
	}
	to, err := mail.ParseAddress(msg.Header.Get("To"))
	if err != nil {
		t.Fatal(err)
	}
	out := relayedMail{to: to.Address}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("content type %q: %v", msg.Header.Get("Content-Type"), err)
	}
	parts := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			body, err = io.ReadAll(quotedprintable.NewReader(part))
		} else {
			body, err = io.ReadAll(part)
		}
		if err != nil {
			t.Fatal(err)
		}
		// SMTP carries CRLF; the mail was kept with the template's LF.
		decoded := strings.ReplaceAll(string(body), "\r\n", "\n")
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch partType {
		case "text/html":
			out.html = decoded
		case "text/plain":
			out.text = decoded
		}
	}
	return out
}

// sendAll starts m with a worker until every row of task is sent, and returns
// what the relay took by recipient.
func sendAll(t *testing.T, db *database.Store, m mailer.Transactional, relay *answeringRelay, task uuid.UUID) map[string]relayedMail {
	t.Helper()
	startMailer(t, m, 1)
	m.Wake()
	waitFor(t, "every row sent", 20*time.Second, func() bool {
		for _, row := range trackedRows(t, db, task) {
			if row.status != "sent" {
				return false
			}
		}
		return true
	})
	out := map[string]relayedMail{}
	for _, raw := range relay.taken() {
		mail := parseRelayed(t, raw)
		out[mail.to] = mail
	}
	return out
}

// A tracked mail goes out with the pixel right after <body> and the links
// every recipient has pointing at /t/c with the recipient's token; personal
// links, mail addresses, unsubscribe and data-skymail-notrack links are as
// they were, and nothing else is added: no sentence, no /t/x link (T8). The
// plain text is as kept. The mail kept (body_html) is byte for byte what was
// rendered, before the send and after it; an untracked recipient's mail is
// exactly it. The relay's acceptance is recorded in sent_at.
func TestATrackedMailGoesOutWithThePixelAndItsLinks(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, trackingOn(coreSays()))
	plainSMTP(m)

	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf()[:2], Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := trackedRows(t, db, task)
	links := linksOf(t, db, task)
	if len(links) != 1 {
		t.Fatalf("links %+v", links)
	}

	mails := sendAll(t, db, m, relay, task)
	after := trackedRows(t, db, task)

	ayseRow, ayseMail := after["ayse@example.com"], mails["ayse@example.com"]
	pixel := tracking.PixelHTML(tracking.PixelURL(trackingBase, ayseRow.token))
	click := tracking.ClickURL(trackingBase, ayseRow.token, links[0].token)
	if !strings.Contains(ayseMail.html, `<body style="margin:0">`+pixel) {
		t.Errorf("Ayşe's mail has no pixel right after <body>:\n%s", ayseMail.html)
	}
	if n := strings.Count(ayseMail.html, `href="`+click+`"`); n != 2 {
		t.Errorf("Ayşe's mail points %d links at %s, want both of the shared link's", n, click)
	}
	for _, kept := range []string{
		`<a href="https://skylab.example/form?e=ayse%40example.com">Sana özel</a>`,
		`<a href="mailto:yk@example.com">Yaz</a>`,
		`<a href="https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=abc">Çık</a>`,
		`<a href="https://skylab.example/gizli" data-skymail-notrack>Sayılmaz</a>`,
	} {
		if !strings.Contains(ayseMail.html, kept) {
			t.Errorf("Ayşe's mail lacks %s", kept)
		}
	}
	// Taken back out, the pixel and the click addresses leave the mail kept.
	restored := strings.Replace(ayseMail.html, pixel, "", 1)
	restored = strings.ReplaceAll(restored, `<a href="`+click+`">`, `<a href="https://skylab.example/etkinlik?gun=1&amp;saat=2">`)
	if restored != ayseRow.bodyHTML {
		t.Errorf("Ayşe's mail is more than the kept mail, the pixel and the links:\n got %q\nwant %q", restored, ayseRow.bodyHTML)
	}
	for _, never := range []string{"/t/x", "takibi", "izleme"} {
		if strings.Contains(ayseMail.html, never) || strings.Contains(ayseMail.text, never) {
			t.Errorf("Ayşe's mail has %q", never)
		}
	}
	if strings.TrimSpace(ayseMail.text) != strings.TrimSpace(ayseRow.body) {
		t.Errorf("Ayşe's plain text = %q, want it as kept %q", ayseMail.text, ayseRow.body)
	}

	mehmetMail, mehmetRow := mails["mehmet@example.com"], after["mehmet@example.com"]
	if mehmetMail.html != mehmetRow.bodyHTML {
		t.Errorf("Mehmet, not tracked, got more than his mail:\n got %q\nwant %q", mehmetMail.html, mehmetRow.bodyHTML)
	}

	for email, row := range after {
		if row.bodyHTML != before[email].bodyHTML || !bytes.Equal(row.token, before[email].token) {
			t.Errorf("%s: the kept mail or the token changed when it went out", email)
		}
		if row.sentAt == nil || row.disabledAt != nil {
			t.Errorf("%s: sent_at %v, tracking_disabled_at %v; want the relay's acceptance and an open token", email, row.sentAt, row.disabledAt)
		}
	}
}

// A token closed before its mail goes out (withdrawal, erasure) sends the
// mail clean.
func TestAClosedTokensMailGoesOutClean(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, trackingOn(coreSays()))
	plainSMTP(m)
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf()[:1], Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn.Exec(context.Background(), `UPDATE mail_queue SET tracking_disabled_at = now() WHERE task_id = $1`, task); err != nil {
		t.Fatal(err)
	}
	mails := sendAll(t, db, m, relay, task)
	row := trackedRows(t, db, task)["ayse@example.com"]
	if got := mails["ayse@example.com"].html; got != row.bodyHTML {
		t.Fatalf("a closed token's mail went out as %q, want the kept mail", got)
	}
}

// The flag turned off between queueing and sending (a redeploy): the mail
// goes out clean and its token is closed for good, saying flag_off.
func TestTrackingTurnedOffBeforeTheMailGoesOut(t *testing.T) {
	captureMailerLogs(t)
	db := trackingQueue(t)
	relay := startAnsweringRelay(t, "", "")
	queued := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, trackingOn(coreSays()))
	task, err := queued.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: announcementTemplate, BodyVariables: []byte(`{}`),
		Recipients: groupOf()[:1], Tracking: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	off := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(off)
	mails := sendAll(t, db, off, relay, task)
	row := trackedRows(t, db, task)["ayse@example.com"]
	if got := mails["ayse@example.com"].html; got != row.bodyHTML {
		t.Fatalf("with the flag off the mail went out as %q, want the kept mail", got)
	}
	if row.token == nil || row.disabledAt == nil || row.reason != tracking.SkipFlagOff {
		t.Fatalf("token %x, closed at %v, reason %q; want the token kept, closed, flag_off", row.token, row.disabledAt, row.reason)
	}
}
