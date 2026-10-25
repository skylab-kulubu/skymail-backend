package bouncereader_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/bouncereader"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const (
	fqdn    = "mail.example.org"
	mailbox = "hesap@bildirim.example.org"
	task    = "40000000-0000-4000-8000-000000000001"
	sentAt  = "2026-10-20T09:59:59.700Z"
	// Graph's receivedDateTime of every item: after the rows were sent.
	receivedAt = "2026-10-20T10:00:01Z"
)

// The rows the dsn package's fixtures name.
const (
	notFoundRow = "6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b" // Yok@std.example.org, 5.1.10
	edgeRow     = "0b3e6c1a-2d4f-4a7b-8c9d-e0f1a2b3c4d5" // mezun@std.example.org, 5.4.1 AS(201806281)
	delayedRow  = "2a7f0e4c-91b3-4d6e-a5f8-3c2b1a0d9e87" // uye@uzak.example.net, delayed: still pending here
	fullRow     = "9c8b7a6d-5e4f-4321-8fed-cba987654321" // dolu@uzak.example.net, 5.2.2
	aliasRow    = "d4c3b2a1-0f9e-4d8c-b7a6-a5b4c3d2e1f0" // takma-ad@std.example.org
)

// The addresses that must never reach a log line.
var addresses = []string{"yok@", "mezun@", "uye@", "dolu@", "takma-ad@", "std.example.org", "uzak.example.net"}

func store(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES ('`+task+`', 'service-account-keycloak-mailer', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, sent_at) VALUES
		    ('`+notFoundRow+`', '`+task+`', 'Yok', 'Yok@Std.Example.org', 'Giriş kodun', 'x', 'sent', '`+sentAt+`'),
		    ('`+edgeRow+`', '`+task+`', 'Mezun', 'mezun@std.example.org', 'Duyuru', 'x', 'sent', '`+sentAt+`'),
		    ('`+delayedRow+`', '`+task+`', 'Üye', 'uye@uzak.example.net', 'Toplantı', 'x', 'pending', NULL),
		    ('`+fullRow+`', '`+task+`', 'Dolu', 'dolu@uzak.example.net', 'Duyuru', 'x', 'sent', '`+sentAt+`'),
		    ('`+aliasRow+`', '`+task+`', 'Takma', 'takma-ad@std.example.org', 'Duyuru', 'x', 'sent', '`+sentAt+`')`); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "dsn", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// item is one message of the fake mailbox: its Graph id, its
// internetMessageId and its MIME (nil: Graph says 404).
type item struct {
	id, messageID string
	mime          []byte
	removed       bool
}

// fakeGraph is a mailbox's Inbox as Graph serves it: the delta in pages of
// pageSize, then a delta link; what is added after a delta link was handed
// out comes with the next delta. It takes only the tokens it is told to.
type fakeGraph struct {
	t        *testing.T
	server   *httptest.Server
	pageSize int

	mu       sync.Mutex
	items    []item
	links    int            // delta links handed out
	upTo     map[string]int // delta token → how many items it had seen
	refuse   map[string]bool
	gone     map[string]bool // delta tokens Graph no longer knows (410)
	foreign  bool            // the next delta page links elsewhere
	requests []string
}

func newFakeGraph(t *testing.T, items ...item) *fakeGraph {
	g := &fakeGraph{t: t, pageSize: 3, items: items, upTo: map[string]int{}, refuse: map[string]bool{}, gone: map[string]bool{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGraph) base() string { return g.server.URL + "/v1.0" }

func (g *fakeGraph) add(items ...item) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.items = append(g.items, items...)
}

func (g *fakeGraph) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

func (g *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.URL.RequestURI())
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || g.refuse[token] {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken","message":"Access token has expired for yok@std.example.org"}}`))
		return
	}
	prefix := "/v1.0/users/" + mailbox
	switch {
	case r.URL.Path == prefix+"/mailFolders/inbox/messages/delta":
		g.delta(w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/messages/") && strings.HasSuffix(r.URL.Path, "/$value"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"/messages/"), "/$value")
		for _, it := range g.items {
			if it.id == id && it.mime != nil {
				_, _ = w.Write(it.mime)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ErrorItemNotFound"}}`))
	default:
		g.t.Errorf("unexpected request %s", r.URL.RequestURI())
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (g *fakeGraph) delta(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	start := 0
	switch {
	case g.gone[query.Get("$deltatoken")]:
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"SyncStateNotFound"}}`))
		return
	case query.Get("$deltatoken") != "":
		start = g.upTo[query.Get("$deltatoken")]
	case query.Get("$skiptoken") != "":
		_, _ = fmt.Sscanf(query.Get("$skiptoken"), "%d", &start)
	default:
		if query.Get("$select") != "internetMessageId,receivedDateTime" {
			g.t.Errorf("the first delta selects %q", query.Get("$select"))
		}
	}
	end := min(start+g.pageSize, len(g.items))
	var value []map[string]any
	for _, it := range g.items[start:end] {
		entry := map[string]any{"id": it.id, "internetMessageId": it.messageID, "receivedDateTime": receivedAt}
		if it.removed {
			entry = map[string]any{"id": it.id, "@removed": map[string]string{"reason": "deleted"}}
		}
		value = append(value, entry)
	}
	page := map[string]any{"value": value}
	link := g.base() + "/users/" + mailbox + "/mailFolders/inbox/messages/delta"
	switch {
	case g.foreign:
		page["@odata.nextLink"] = "https://graph.example.net/v1.0/users/" + mailbox + "/mailFolders/inbox/messages/delta?$skiptoken=3"
	case end < len(g.items):
		page["@odata.nextLink"] = fmt.Sprintf("%s?$skiptoken=%d", link, end)
	default:
		g.links++
		token := fmt.Sprintf("d%d", g.links)
		g.upTo[token] = end
		page["@odata.deltaLink"] = link + "?$deltatoken=" + token
	}
	_ = json.NewEncoder(w).Encode(page)
}

// fakeTokens hands out token-1, token-2, …: a new one after the current one
// is dropped.
type fakeTokens struct {
	mu      sync.Mutex
	n       int
	current string
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == "" {
		f.n++
		f.current = fmt.Sprintf("token-%d", f.n)
	}
	return f.current, nil
}

func (f *fakeTokens) Invalidate(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token == f.current {
		f.current = ""
	}
}

func newReader(db *database.Store, g *fakeGraph, mode bouncereader.Mode, logs *bytes.Buffer) *bouncereader.Reader {
	return bouncereader.New(db, fqdn, bouncereader.Config{Mode: mode, Mailboxes: []string{mailbox}},
		bouncereader.WithGraph(g.base(), &fakeTokens{}, nil),
		bouncereader.WithLogger(zerolog.New(logs)))
}

type state struct {
	delta                                                                       *string
	lastError                                                                   *string
	reports, notDSN, unmatched, hard, soft, delay, other, duplicate, unreadable int64
}

func stateOf(t *testing.T, db *database.Store, mode string) state {
	t.Helper()
	var s state
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT delta_link, last_error, reports, not_dsn, unmatched, hard, soft, delay, other_recipient, duplicate, unreadable
		FROM mail_bounce_reader_state WHERE mailbox = $1 AND mode = $2`, mailbox, mode).
		Scan(&s.delta, &s.lastError, &s.reports, &s.notDSN, &s.unmatched, &s.hard, &s.soft, &s.delay, &s.other, &s.duplicate, &s.unreadable); err != nil {
		t.Fatalf("state of %s: %v", mode, err)
	}
	return s
}

func suppressed(t *testing.T, db *database.Store) []string {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(), `SELECT address FROM mail_suppressions ORDER BY address`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		all = append(all, a)
	}
	return all
}

func count(t *testing.T, db *database.Store, sql string) int {
	t.Helper()
	var n int
	if err := db.Conn.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The mailbox of the tests: a permanent failure, a full mailbox, an
// auto-reply, the rotator's alarm (another Message-ID domain), a delay of a
// row that was never sent, a report naming an address the row never had, an
// item deleted since, and one Graph no longer finds.
func inbox(t *testing.T) []item {
	otherRecipient := bytes.Replace(fixture(t, "exchange-several-recipients.eml"),
		[]byte("Original-Recipient: rfc822;takma-ad@std.example.org\n"), nil, 1)
	return []item{
		{id: "AAMk-1", messageID: "<ndr-1@outlook.example>", mime: fixture(t, "exchange-recipient-not-found.eml")},
		{id: "AAMk-2", messageID: "<ndr-2@outlook.example>", mime: fixture(t, "remote-mailbox-full.eml")},
		{id: "AAMk-3", messageID: "<ndr-3@outlook.example>", mime: fixture(t, "auto-reply.eml")},
		{id: "AAMk-4", messageID: "<ndr-4@outlook.example>", mime: fixture(t, "rotator-alarm.eml")},
		{id: "AAMk-5", messageID: "<ndr-5@outlook.example>", mime: fixture(t, "exchange-delayed.eml")},
		{id: "AAMk-6", messageID: "<ndr-6@outlook.example>", mime: otherRecipient},
		{id: "AAMk-7", removed: true},
		{id: "AAMk-8", messageID: "<ndr-8@outlook.example>"},
	}
}

// On, a poll reads the whole Inbox page by page, records every report and
// suppresses only the address a permanent failure of its own row was
// reported for: not the row that was never sent, not the address the row
// never had, nothing for the auto-reply or the rotator's alarm. The next poll
// starts from the delta link and reads only what is new. No log line holds
// an address.
func TestOnAPollRecordsTheReportsAndSuppressesOnlyAPermanentFailure(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	var logs bytes.Buffer
	reader := newReader(db, g, bouncereader.ModeOn, &logs)
	if err := reader.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if got := suppressed(t, db); strings.Join(got, " ") != "yok@std.example.org" {
		t.Errorf("suppressed = %v, want only yok@std.example.org", got)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces`); n != 3 {
		t.Errorf("bounces = %d, want 3 (hard, soft, another address)", n)
	}
	s := stateOf(t, db, "on")
	if s.reports != 6 || s.hard != 1 || s.soft != 1 || s.notDSN != 1 || s.unmatched != 2 || s.other != 1 || s.lastError != nil {
		t.Errorf("state = %+v", s)
	}
	if s.delta == nil || !strings.HasSuffix(*s.delta, "$deltatoken=d1") {
		t.Errorf("delta link = %v", s.delta)
	}

	// What comes later: Exchange's Directory-Based Edge Blocking report.
	g.add(item{id: "AAMk-9", messageID: "<ndr-9@outlook.example>", mime: fixture(t, "exchange-edge-blocking.eml")})
	before := len(g.seen())
	if err := reader.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	later := g.seen()[before:]
	if len(later) != 2 || !strings.Contains(later[0], "$deltatoken=d1") || !strings.Contains(later[1], "/messages/AAMk-9/$value") {
		t.Errorf("the second poll asked %v, want the delta from d1 and the new item", later)
	}
	if got := suppressed(t, db); strings.Join(got, " ") != "mezun@std.example.org yok@std.example.org" {
		t.Errorf("suppressed = %v", got)
	}
	if s := stateOf(t, db, "on"); s.reports != 7 || s.hard != 2 || !strings.HasSuffix(*s.delta, "$deltatoken=d2") {
		t.Errorf("state after the second poll = %+v", s)
	}

	for _, address := range addresses {
		if strings.Contains(strings.ToLower(logs.String()), address) {
			t.Errorf("a log line holds %q:\n%s", address, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"kalici":1`) {
		t.Errorf("the poll's counts are not logged:\n%s", logs.String())
	}
}

// A dry run counts what each report would do and writes nothing else: no
// bounce, no suppression. Its place in the mailbox is its own: turned on,
// the reader reads the Inbox again from the start and records it.
func TestADryRunCountsAndWritesNothingAndOnReadsAgainFromTheStart(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	var logs bytes.Buffer
	if err := newReader(db, g, bouncereader.ModeDryRun, &logs).Poll(context.Background()); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := count(t, db, `SELECT (SELECT count(*) FROM mail_bounces) + (SELECT count(*) FROM mail_suppressions)`); n != 0 {
		t.Errorf("a dry run wrote %d rows", n)
	}
	if s := stateOf(t, db, "dry-run"); s.reports != 6 || s.hard != 1 || s.soft != 1 || s.other != 1 {
		t.Errorf("dry-run state = %+v", s)
	}

	before := len(g.seen())
	if err := newReader(db, g, bouncereader.ModeOn, &logs).Poll(context.Background()); err != nil {
		t.Fatalf("on: %v", err)
	}
	if first := g.seen()[before]; strings.Contains(first, "$deltatoken") {
		t.Errorf("on started from the dry run's delta link: %s", first)
	}
	if got := suppressed(t, db); strings.Join(got, " ") != "yok@std.example.org" {
		t.Errorf("suppressed = %v", got)
	}
}

// Graph's 401 drops the token and stops the poll where it was; the error
// kept is the status and code, never Graph's message. The next poll takes a
// new token and reads from the same place.
func TestA401DropsTheTokenAndTheNextPollReadsFromTheSamePlace(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	g.refuse["token-1"] = true
	var logs bytes.Buffer
	reader := newReader(db, g, bouncereader.ModeOn, &logs)
	if err := reader.Poll(context.Background()); err == nil {
		t.Fatal("Poll: no error after a 401")
	}
	s := stateOf(t, db, "on")
	if s.lastError == nil || *s.lastError != "graph: 401 InvalidAuthenticationToken" || s.delta != nil {
		t.Errorf("state after the 401 = %+v (error %v)", s, s.lastError)
	}
	if err := reader.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if s := stateOf(t, db, "on"); s.lastError != nil || s.hard != 1 || s.delta == nil {
		t.Errorf("state after the second poll = %+v", s)
	}
	if strings.Contains(logs.String(), "yok@") || strings.Contains(logs.String(), "Access token") {
		t.Errorf("Graph's message reached the log:\n%s", logs.String())
	}
}

// A next link to anywhere but Graph is refused: the token never goes there,
// and the poll stops with the delta link it had.
func TestALinkToAnotherHostIsRefused(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	g.foreign = true
	if err := newReader(db, g, bouncereader.ModeOn, &bytes.Buffer{}).Poll(context.Background()); err == nil {
		t.Fatal("Poll: no error for a foreign link")
	}
	if s := stateOf(t, db, "on"); s.lastError == nil || !strings.Contains(*s.lastError, "not Graph's") || s.delta != nil {
		t.Errorf("state = %+v", s)
	}
}

// With several replicas one polls at a time: while another holds the lock, a
// poll asks Graph nothing.
func TestAPollWaitsForNoOneAndSkipsWhileAnotherPolls(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	holding, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := db.WithBounceReaderLock(context.Background(), func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
		done <- err
	}()
	<-holding
	if err := newReader(db, g, bouncereader.ModeOn, &bytes.Buffer{}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if seen := g.seen(); len(seen) != 0 {
		t.Errorf("a poll under another's lock asked Graph %v", seen)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := newReader(db, g, bouncereader.ModeOn, &bytes.Buffer{}).Poll(context.Background()); err != nil {
		t.Fatalf("Poll after the lock: %v", err)
	}
	if len(g.seen()) == 0 {
		t.Error("the poll after the lock asked Graph nothing")
	}
}

// Run polls at once and stops with its context; off, it does nothing.
func TestRunPollsAtOnceAndStopsWithItsContext(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	reader := bouncereader.New(db, fqdn, bouncereader.Config{Mode: bouncereader.ModeOn, Mailboxes: []string{mailbox}},
		bouncereader.WithGraph(g.base(), &fakeTokens{}, nil), bouncereader.WithInterval(time.Hour))
	go func() {
		reader.Run(ctx)
		close(stopped)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(suppressed(t, db)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not poll")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop with its context")
	}

	off := bouncereader.New(db, fqdn, bouncereader.Config{Mode: bouncereader.ModeOff}, bouncereader.WithGraph(g.base(), &fakeTokens{}, nil))
	before := len(g.seen())
	off.Run(context.Background())
	if len(g.seen()) != before {
		t.Error("an off reader asked Graph")
	}
}

// A delta link Graph no longer knows (410) reads the Inbox again from the
// start: what was recorded before is a duplicate, what is new is recorded.
func TestAnExpiredDeltaLinkReadsTheInboxFromTheStart(t *testing.T) {
	db := store(t)
	g := newFakeGraph(t, inbox(t)...)
	reader := newReader(db, g, bouncereader.ModeOn, &bytes.Buffer{})
	if err := reader.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	g.mu.Lock()
	g.gone["d1"] = true
	g.mu.Unlock()
	g.add(item{id: "AAMk-9", messageID: "<ndr-9@outlook.example>", mime: fixture(t, "exchange-edge-blocking.eml")})
	if err := reader.Poll(context.Background()); err != nil {
		t.Fatalf("Poll after the delta link expired: %v", err)
	}
	s := stateOf(t, db, "on")
	if s.lastError != nil || s.hard != 2 || s.duplicate != 3 || s.delta == nil || strings.HasSuffix(*s.delta, "=d1") {
		t.Errorf("state = %+v", s)
	}
	if got := suppressed(t, db); strings.Join(got, " ") != "mezun@std.example.org yok@std.example.org" {
		t.Errorf("suppressed = %v", got)
	}
}
