package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// Open and click tracking's withdrawal and retention (hub ticket
// iletisim-servisi 14c; design note 07 §4.7).

const (
	retentionTemplate          = "10000000-0000-4000-8000-0000000000aa"
	retentionSensitiveTemplate = "10000000-0000-4000-8000-0000000000bb"
)

// retentionStore is a migrated database with two templates that are not
// sensitive: sends of them may be tracked.
func retentionStore(t *testing.T) *Store {
	t.Helper()
	store := lifecycleStore(t)
	if _, err := store.Conn.Exec(context.Background(), `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content, content_sensitive) VALUES
		    ('`+retentionTemplate+`', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', '', false),
		    ('`+retentionSensitiveTemplate+`', 'Sonra hassas', 'Sonra hassas', '<p>x</p>', 'x', '', false)`); err != nil {
		t.Fatal(err)
	}
	return store
}

// recipient is a queue row of a tracked send: its subject, whether it has a
// token, how many days ago its mail went out (negative: still pending), and
// the events it has.
type recipient struct {
	sub     uuid.UUID
	tracked bool
	sentAgo int
	opens   int
	clicks  int
	// automated marks the events automated_likely.
	automated bool
}

type seededSend struct {
	task   uuid.UUID
	rows   []uuid.UUID
	tokens [][]byte
	links  []int64
}

// seedSend writes a send that asked for tracking, created createdAgo days
// ago, its rows and their events, and one link per entry of linkAgo (days
// since the link was written). A click is on the first link.
func seedSend(t *testing.T, s *Store, template string, createdAgo int, linkAgo []int, people ...recipient) seededSend {
	t.Helper()
	ctx := context.Background()
	send := seededSend{task: uuid.New()}
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, tracking_requested, content_sensitive_at_send, created_at)
		VALUES ($1, 'sender', $2, '{}', true, 'false', now() - make_interval(days => $3::int))`, send.task, template, createdAgo); err != nil {
		t.Fatal(err)
	}
	for i, ago := range linkAgo {
		var id int64
		token := make([]byte, 8)
		copy(token, send.task[:7])
		token[7] = byte(i)
		if err := s.Conn.QueryRow(ctx, `
			INSERT INTO mail_task_links (task_id, link_token, url, position, created_at)
			VALUES ($1, $2, $3, $4, now() - make_interval(days => $5::int)) RETURNING id`,
			send.task, token, fmt.Sprintf("https://skylab.example/%d", i), i, ago).Scan(&id); err != nil {
			t.Fatal(err)
		}
		send.links = append(send.links, id)
	}
	for i, p := range people {
		id := uuid.New()
		var token []byte
		var skip *string
		if p.tracked {
			token, _ = tracking.NewRecipientToken()
		} else {
			reason := "no_account"
			skip = &reason
		}
		status, sentAt := "sent", fmt.Sprintf("now() - make_interval(days => %d)", p.sentAgo)
		if p.sentAgo < 0 {
			status, sentAt = "pending", "NULL"
		}
		var sub *uuid.UUID
		if p.sub != uuid.Nil {
			sub = &p.sub
		}
		if _, err := s.Conn.Exec(ctx, `
			INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, recipient_sub, subject, body, status,
			                        sent_at, tracking_token, tracking_skip_reason)
			VALUES ($1, $2, $3, $4, $5, 'Duyuru', 'x', $6, `+sentAt+`, $7, $8)`,
			id, send.task, fmt.Sprintf("Kişi %d", i), fmt.Sprintf("kisi%d-%s@example.com", i, id), sub, status, token, skip); err != nil {
			t.Fatal(err)
		}
		send.rows = append(send.rows, id)
		send.tokens = append(send.tokens, token)
		for e := 0; e < p.opens+p.clicks; e++ {
			kind, link := "open", any(nil)
			if e >= p.opens {
				kind, link = "click", send.links[0]
			}
			if _, err := s.Conn.Exec(ctx, `
				INSERT INTO mail_tracking_events (queue_id, task_id, kind, link_id, occurred_at, source_class, automated_likely, classifier_version)
				VALUES ($1, $2, $3, $4, now() - make_interval(days => $5::int), 'browser', $6, 1)`,
				id, send.task, kind, link, max(p.sentAgo-1, 0), p.automated); err != nil {
				t.Fatal(err)
			}
		}
	}
	return send
}

func tracked(sentAgo, opens, clicks int) recipient {
	return recipient{sub: uuid.New(), tracked: true, sentAgo: sentAgo, opens: opens, clicks: clicks}
}

// trackingState is everything the withdrawal and the retention run may
// change, as text, to compare before and after.
func trackingState(t *testing.T, s *Store) map[string]string {
	t.Helper()
	state := map[string]string{}
	for name, sql := range map[string]string{
		"events": `SELECT id::text, queue_id::text || '|' || kind FROM mail_tracking_events`,
		"queue":  `SELECT id::text, COALESCE(tracking_disabled_at::text, 'open') FROM mail_queue WHERE tracking_token IS NOT NULL`,
		"links":  `SELECT id::text, COALESCE(url, '') || '|' || COALESCE(url_cleared_reason, '') FROM mail_task_links`,
		"totals": `SELECT task_id::text, tracked_recipients::text || '|' || unique_opens::text || '|' || clicks_by_link::text FROM mail_task_tracking_totals`,
		"sends":  `SELECT id::text, COALESCE(tracking_closed_at::text, 'open') FROM mail_tasks`,
	} {
		rows, err := s.Conn.Query(context.Background(), sql)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, value string
			if err := rows.Scan(&id, &value); err != nil {
				t.Fatal(err)
			}
			state[name+" "+id] = value
		}
		rows.Close()
	}
	return state
}

// A withdrawal (core, the person turned tracking off on my.) closes every
// token of mail to the person, sent or still queued, and deletes their
// openings and clicks. Their old mail's links still redirect; nobody else's
// tracking and no count without people changes. A repeat changes nothing.
func TestWithdrawTrackingStopsThePersonsTrackingAndKeepsTheLinks(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	deniz, ayse := tracked(3, 2, 1), tracked(3, 1, 0)
	announcement := seedSend(t, s, retentionTemplate, 4, []int{4}, deniz, ayse)
	queued := recipient{sub: deniz.sub, tracked: true, sentAgo: -1}
	later := seedSend(t, s, retentionTemplate, 0, nil, queued)
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers)
		VALUES ($1, 12, 7, 5, 3)`, announcement.task); err != nil {
		t.Fatal(err)
	}
	before := trackingState(t, s)

	got, err := s.WithdrawTracking(ctx, deniz.sub.String())
	if err != nil {
		t.Fatal(err)
	}
	if want := (TrackingWithdrawal{TokensClosed: 2, EventsDeleted: 3}); got != want {
		t.Errorf("withdrawal = %+v, want %+v", got, want)
	}

	after := trackingState(t, s)
	for key, value := range before {
		changed := after[key] != value
		mine := strings.Contains(key, announcement.rows[0].String()) || strings.Contains(key, later.rows[0].String())
		switch {
		case strings.HasPrefix(key, "events ") && strings.HasPrefix(value, announcement.rows[0].String()):
			if _, kept := after[key]; kept {
				t.Errorf("Deniz's %s is kept", key)
			}
		case strings.HasPrefix(key, "queue ") && mine:
			if after[key] == "open" {
				t.Errorf("Deniz's token %s is open", key)
			}
		case changed:
			t.Errorf("%s changed: %q -> %q", key, value, after[key])
		}
	}
	if len(after) != len(before)-3 {
		t.Errorf("%d rows after, want %d", len(after), len(before)-3)
	}

	// Deniz's mail records nothing more; Ayşe's does.
	if written, err := s.RecordTracking(ctx, announcement.tokens[0], 0, "Mozilla/5.0 Chrome/120", "GET"); written || !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("Deniz's pixel after withdrawal: %v %v", written, err)
	}
	if written, err := s.RecordTracking(ctx, announcement.tokens[1], 0, "Mozilla/5.0 Chrome/120", "GET"); !written || err != nil {
		t.Errorf("Ayşe's pixel after Deniz's withdrawal: %v %v", written, err)
	}
	link, err := s.ResolveTrackingLink(ctx, linkToken(t, s, announcement.links[0]))
	if err != nil || link.Url == nil {
		t.Errorf("the link after withdrawal: %+v %v; want it to redirect", link, err)
	}

	again, err := s.WithdrawTracking(ctx, deniz.sub.String())
	if err != nil || again != (TrackingWithdrawal{}) {
		t.Errorf("repeat = %+v, %v; want nothing changed", again, err)
	}
}

func linkToken(t *testing.T, s *Store, id int64) []byte {
	t.Helper()
	var token []byte
	if err := s.Conn.QueryRow(context.Background(), `SELECT link_token FROM mail_task_links WHERE id = $1`, id).Scan(&token); err != nil {
		t.Fatal(err)
	}
	return token
}

// waitForLockWait waits until a statement on a table waits for a lock.
func waitForLockWait(t *testing.T, s *Store, like string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for queryCount(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, like) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("nothing waited for a lock on %s", like)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A click writer holds the recipient's queue row and then the link (the
// event's foreign key). A withdrawal takes the person's queue rows first, so
// it waits for the click to commit and then deletes it: no event of the
// person survives the withdrawal, and the click never waits behind it.
func TestTrackingWithdrawalWaitsForAnInFlightClick(t *testing.T) {
	s := retentionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deniz := tracked(1, 0, 0)
	send := seedSend(t, s, retentionTemplate, 1, []int{1}, deniz)

	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	q := s.WithTx(tx)
	locked, err := q.LockTrackingRecipient(ctx, send.tokens[0])
	if err != nil {
		t.Fatal(err)
	}
	withdrawn := make(chan error, 1)
	go func() {
		_, err := s.WithdrawTracking(ctx, deniz.sub.String())
		withdrawn <- err
	}()
	waitForLockWait(t, s, "%mail_queue%")

	clickCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	count, err := q.InsertTrackingEvent(clickCtx, InsertTrackingEventParams{QueueID: send.rows[0], Kind: "click",
		LinkID: pgtype.Int8{Int64: send.links[0], Valid: true}, At: locked.At, SourceClass: "browser", ClassifierVersion: 1})
	stop()
	if err != nil || count != 1 {
		t.Fatalf("in-flight click: %d %v", count, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-withdrawn; err != nil {
		t.Fatal(err)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id = $1`, send.rows[0]); n != 0 {
		t.Errorf("%d events of the person survived the withdrawal", n)
	}
}

func retentionCounts(r *TrackingRetentionReport) TrackingRetentionCounts {
	if r == nil {
		return TrackingRetentionCounts{}
	}
	return r.Counts
}

// The retention run (SKYMAIL_TRACKING_RETENTION). 90 days (EventRetention)
// after a recipient's mail went out, their send's counts without people are
// kept — every send's (KVKK notice s6), one of fewer than 10 tracked
// recipients only within a year of its creation — then their token closes
// and their events go; a send all of whose mail is past it closes. The counts
// of a send of fewer than 10 go a year after it was created. A link goes a
// year after it was written (expired), and at once when its send is masked
// or sensitive now (D7, erased). A dry run counts exactly what apply then
// changes, from the same statements, and changes nothing.
func TestTrackingRetentionDryRunCountsWhatApplyChanges(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	day := tracking.EventRetentionDays

	// Ten tracked and one untracked, all 90 days and a day ago, in a send
	// created a year and a day ago whose links are as old.
	var people []recipient
	for i := 0; i < 10; i++ {
		people = append(people, tracked(day+1, 1, i%2))
	}
	people[0].opens, people[0].automated = 2, true
	people = append(people, recipient{sentAgo: day + 1})
	big := seedSend(t, s, retentionTemplate, 366, []int{366, 366}, people...)
	// Nine tracked: their counts are kept too, for the send's first year.
	var nine []recipient
	for i := 0; i < 9; i++ {
		nine = append(nine, tracked(day+1, 1, 0))
	}
	small := seedSend(t, s, retentionTemplate, day+1, nil, nine...)
	// One recipient a day short of the 90 days, one a day past them.
	edge := seedSend(t, s, retentionTemplate, day+1, []int{day - 1}, tracked(day-1, 1, 1), tracked(day+1, 1, 1))
	// Three tracked, a year and a day ago, retention never ran on them: no
	// counts are kept, the year of a send of fewer than 10 is over.
	late := seedSend(t, s, retentionTemplate, 366, nil, tracked(366, 1, 0), tracked(366, 1, 0), tracked(366, 1, 0))
	// Three tracked, a year and a day ago, rolled up and closed long since:
	// their counts go now.
	old := seedSend(t, s, retentionTemplate, 366, nil, tracked(366, 0, 0), tracked(366, 0, 0), tracked(366, 0, 0))
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_queue SET tracking_disabled_at = now() - interval '275 days' WHERE task_id = $1`, old.task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_tasks SET tracking_closed_at = now() - interval '275 days' WHERE id = $1`, old.task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers, rolled_up_at)
		VALUES ($1, 3, 2, 2, 1, now() - interval '275 days')`, old.task); err != nil {
		t.Fatal(err)
	}
	// A recent send masked since (D7), and one whose template turned sensitive.
	masked := seedSend(t, s, retentionTemplate, 10, []int{10}, tracked(10, 1, 1))
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_tasks SET content_masked_at = now() WHERE id = $1`, masked.task); err != nil {
		t.Fatal(err)
	}
	sensitive := seedSend(t, s, retentionSensitiveTemplate, 10, []int{10}, tracked(10, 1, 0))
	if _, err := s.Conn.Exec(ctx, `UPDATE templates SET content_sensitive = true WHERE id = $1`, retentionSensitiveTemplate); err != nil {
		t.Fatal(err)
	}
	// A recent send: nothing to do.
	recent := seedSend(t, s, retentionTemplate, 10, []int{10}, tracked(10, 2, 1))
	before := trackingState(t, s)

	want := TrackingRetentionCounts{
		Sends:         7,
		TotalsWritten: 3, // big, small, edge
		TotalsDeleted: 1, // old
		TokensClosed:  10 + 9 + 1 + 3,
		EventsDeleted: (11 + 5) + 9 + 2 + 3,
		LinksExpired:  2,
		LinksErased:   2,
		SendsClosed:   3, // big, small, late
	}

	dry, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionDryRun})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Status != "ok" || dry.Counts != want {
		t.Errorf("dry run = %s %+v\nwant ok %+v", dry.Status, dry.Counts, want)
	}
	if after := trackingState(t, s); !reflect.DeepEqual(after, before) {
		t.Errorf("the dry run changed rows:\nbefore %v\nafter  %v", before, after)
	}
	assertReceipt(t, s, dry)

	apply, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
	if err != nil {
		t.Fatal(err)
	}
	if apply.Status != "ok" || apply.Counts != dry.Counts {
		t.Errorf("apply = %s %+v\nwant ok and the dry run's %+v", apply.Status, apply.Counts, dry.Counts)
	}
	assertReceipt(t, s, apply)

	// What stays: the edge's recipient a day short of the 90 days, the masked
	// and sensitive sends' events (their links went), the recent send.
	for _, kept := range []uuid.UUID{edge.rows[0], masked.rows[0], sensitive.rows[0], recent.rows[0]} {
		if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id = $1`, kept); n == 0 {
			t.Errorf("events of %s went", kept)
		}
		if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE id = $1 AND tracking_disabled_at IS NULL`, kept); n != 1 {
			t.Errorf("token of %s closed", kept)
		}
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events`); n != 2+1+2+3 {
		t.Errorf("%d events left, want 8", n)
	}
	for id, wantState := range map[int64]string{
		big.links[0]: "|expired", big.links[1]: "|expired", edge.links[0]: "https://skylab.example/0|",
		masked.links[0]: "|erased", sensitive.links[0]: "|erased", recent.links[0]: "https://skylab.example/0|",
	} {
		if got := queryString(t, s, `SELECT COALESCE(url, '') || '|' || COALESCE(url_cleared_reason, '') FROM mail_task_links WHERE id = $1`, id); got == nil || *got != wantState {
			t.Errorf("link %d = %v, want %q", id, got, wantState)
		}
	}
	for task, closed := range map[uuid.UUID]bool{big.task: true, small.task: true, late.task: true, old.task: true, edge.task: false, masked.task: false, recent.task: false} {
		if n := queryCount(t, s, `SELECT count(*) FROM mail_tasks WHERE id = $1 AND tracking_closed_at IS NOT NULL`, task); (n == 1) != closed {
			t.Errorf("send %s closed = %v, want %v", task, n == 1, closed)
		}
	}

	// The counts without people of the send of ten, as its live report had
	// them; the send of nine's and the edge's as theirs; none for the sends
	// of three a year old.
	report, err := s.GetRetainedTrackingReport(ctx, big.task)
	if err != nil {
		t.Fatal(err)
	}
	var totals map[string]any
	if err := json.Unmarshal(report, &totals); err != nil {
		t.Fatal(err)
	}
	delete(totals, "rolled_up_at")
	wantTotals := map[string]any{
		"tracked_recipients":        float64(10),
		"untracked_by_reason":       map[string]any{"no_account": float64(1)},
		"unique_opens":              float64(10),
		"unique_likely_human_opens": float64(9),
		"unique_clickers":           float64(5),
		"clicks_by_link": []any{
			map[string]any{"id": float64(big.links[0]), "position": float64(0), "clicks": float64(5), "unique_clickers": float64(5)},
			map[string]any{"id": float64(big.links[1]), "position": float64(1), "clicks": float64(0), "unique_clickers": float64(0)},
		},
	}
	if !reflect.DeepEqual(totals, wantTotals) {
		t.Errorf("totals = %v\nwant     %v", totals, wantTotals)
	}
	for task, want := range map[uuid.UUID]int{small.task: 9, edge.task: 2} {
		if n := queryCount(t, s, `SELECT tracked_recipients FROM mail_task_tracking_totals WHERE task_id = $1`, task); n != want {
			t.Errorf("send %s keeps counts of %d, want %d", task, n, want)
		}
	}
	if n := queryCount(t, s, `SELECT unique_opens FROM mail_task_tracking_totals WHERE task_id = $1`, small.task); n != 9 {
		t.Errorf("send of nine: unique_opens = %d, want 9", n)
	}
	for _, task := range []uuid.UUID{late.task, old.task} {
		if _, err := s.GetRetainedTrackingReport(ctx, task); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("send of three a year old has totals: %v", err)
		}
	}

	again, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts.TotalsWritten+again.Counts.TotalsDeleted+again.Counts.TokensClosed+again.Counts.EventsDeleted+again.Counts.LinksExpired+again.Counts.LinksErased+again.Counts.SendsClosed != 0 {
		t.Errorf("a second apply changed %+v", again.Counts)
	}
}

// The counts of a send are kept from all its events the first time one of
// its recipients is due, before any of them goes; the later recipients'
// events then go one by one, the counts staying as they were.
func TestTrackingRetentionKeepsTheCountsBeforeTheFirstEventGoes(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	day := tracking.EventRetentionDays
	var people []recipient
	for i := 0; i < 10; i++ {
		people = append(people, tracked(day-1, 1, 0))
	}
	people[0].sentAgo = day + 1
	send := seedSend(t, s, retentionTemplate, day+1, nil, people...)

	got, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts.TotalsWritten != 1 || got.Counts.EventsDeleted != 1 || got.Counts.TokensClosed != 1 || got.Counts.SendsClosed != 0 {
		t.Fatalf("counts = %+v, want the totals, one event and one token", got.Counts)
	}
	if n := queryCount(t, s, `SELECT unique_opens FROM mail_task_tracking_totals WHERE task_id = $1`, send.task); n != 10 {
		t.Errorf("unique_opens = %d, want all 10", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE task_id = $1`, send.task); n != 9 {
		t.Errorf("%d events left, want the 9 within their 90 days", n)
	}
}

func assertReceipt(t *testing.T, s *Store, report *TrackingRetentionReport) {
	t.Helper()
	var mode, status string
	var raw []byte
	var finished bool
	if err := s.Conn.QueryRow(context.Background(), `
		SELECT mode, status, counts, finished_at IS NOT NULL FROM tracking_retention_runs WHERE id = $1`, report.RunID).Scan(&mode, &status, &raw, &finished); err != nil {
		t.Fatal(err)
	}
	var counts TrackingRetentionCounts
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatal(err)
	}
	if mode != string(report.Mode) || status != report.Status || counts != report.Counts || !finished {
		t.Errorf("receipt = %s %s %+v finished %v; want %s %s %+v", mode, status, counts, finished, report.Mode, report.Status, report.Counts)
	}
}

// The brake: an apply that would delete or close more than BrakeMaxRows
// changes nothing and says so (a wrong clock must never empty the table at
// once). The dry run reports what the brake would refuse.
func TestTrackingRetentionBrakeRefusesALargeRun(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	day := tracking.EventRetentionDays
	seedSend(t, s, retentionTemplate, day+1, nil, tracked(day+1, 2, 0), tracked(day+1, 1, 0))
	before := trackingState(t, s)

	for mode, status := range map[tracking.RetentionMode]string{tracking.RetentionDryRun: "would_refuse_large", tracking.RetentionApply: "refused_large"} {
		got, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: mode, BrakeMaxRows: 2})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status || got.Counts.EventsDeleted != 3 {
			t.Errorf("%s = %s %+v, want %s counting 3 events", mode, got.Status, got.Counts, status)
		}
		assertReceipt(t, s, got)
	}
	if after := trackingState(t, s); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused run changed rows:\nbefore %v\nafter  %v", before, after)
	}
}

// One run at a time across replicas: a run that finds the lock held does
// nothing and writes no receipt.
func TestTrackingRetentionRunsOneAtATime(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	conn, err := s.Conn.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, TrackingRetentionLockName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply}); !errors.Is(err, ErrTrackingRetentionLocked) {
		t.Fatalf("run while locked: %v, want ErrTrackingRetentionLocked", err)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM tracking_retention_runs`); n != 0 {
		t.Errorf("%d receipts for a run that did not run", n)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, TrackingRetentionLockName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply}); err != nil {
		t.Fatalf("run after the lock went: %v", err)
	}
}

// A run is due when no run of its mode ended in the last 23 hours; a failed
// run does not count, so the next hour tries again.
func TestTrackingRetentionDue(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	due := func(mode tracking.RetentionMode) bool {
		t.Helper()
		ok, err := s.TrackingRetentionDue(ctx, mode)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !due(tracking.RetentionDryRun) || !due(tracking.RetentionApply) {
		t.Fatal("nothing ran yet, and a run is not due")
	}
	run, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionDryRun})
	if err != nil {
		t.Fatal(err)
	}
	if due(tracking.RetentionDryRun) || !due(tracking.RetentionApply) {
		t.Error("after a dry run: dry-run due or apply not due")
	}
	if _, err := s.Conn.Exec(ctx, `UPDATE tracking_retention_runs SET started_at = now() - interval '23 hours 1 minute' WHERE id = $1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if !due(tracking.RetentionDryRun) {
		t.Error("23 hours on, the dry run is not due")
	}
	if _, err := s.Conn.Exec(ctx, `UPDATE tracking_retention_runs SET started_at = now(), status = 'failed' WHERE id = $1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if !due(tracking.RetentionDryRun) {
		t.Error("after a failed run, the next is not due")
	}
}

// purge, the full roll-back: every event goes and every token closes,
// whatever their age; the links and the counts without people stay.
func TestTrackingRetentionPurgeDeletesEveryEvent(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	recent := seedSend(t, s, retentionTemplate, 2, []int{2}, tracked(1, 2, 1), tracked(1, 1, 0), recipient{sub: uuid.New(), tracked: true, sentAgo: -1})
	if _, err := s.Conn.Exec(ctx, `
		INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers)
		VALUES ($1, 12, 7, 5, 3)`, recent.task); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionPurge, BrakeMaxRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.Counts.EventsDeleted != 4 || got.Counts.TokensClosed != 3 {
		t.Errorf("purge = %s %+v, want ok, 4 events and 3 tokens (no brake)", got.Status, got.Counts)
	}
	assertReceipt(t, s, got)
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events`); n != 0 {
		t.Errorf("%d events after purge", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_queue WHERE tracking_token IS NOT NULL AND tracking_disabled_at IS NULL`); n != 0 {
		t.Errorf("%d tokens open after purge", n)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_task_links WHERE url IS NOT NULL`); n != 1 {
		t.Error("purge emptied the link")
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_task_tracking_totals`); n != 1 {
		t.Error("purge deleted the counts without people")
	}
}

// The retention run empties links (expired, D7) of a send another recipient
// of which may be clicking: it takes the send's queue rows before any link,
// so it waits for the click, and the click never waits behind its link lock.
func TestTrackingRetentionWaitsForAnInFlightClick(t *testing.T) {
	s := retentionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	send := seedSend(t, s, retentionTemplate, 1, []int{1}, tracked(1, 0, 0))
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_tasks SET content_masked_at = now() WHERE id = $1`, send.task); err != nil {
		t.Fatal(err)
	}

	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	q := s.WithTx(tx)
	locked, err := q.LockTrackingRecipient(ctx, send.tokens[0])
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() {
		_, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
		ran <- err
	}()
	waitForLockWait(t, s, "%mail_queue%")

	clickCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	count, err := q.InsertTrackingEvent(clickCtx, InsertTrackingEventParams{QueueID: send.rows[0], Kind: "click",
		LinkID: pgtype.Int8{Int64: send.links[0], Valid: true}, At: locked.At, SourceClass: "browser", ClassifierVersion: 1})
	stop()
	if err != nil || count != 1 {
		t.Fatalf("in-flight click: %d %v", count, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_task_links WHERE id = $1 AND url IS NULL AND url_cleared_reason = 'erased'`, send.links[0]); n != 1 {
		t.Error("the masked send's link was not emptied")
	}
}

// T4 counts people a count could tell about: the ≥10 threshold and
// tracked_recipients count only the tracked mail that went out and whose
// token is still open (not withdrawn, erased or closed). A send of ten
// tokens one of which failed keeps counts of nine, which go a year after
// the send; one of eleven with one withdrawal keeps them for ten, the
// withdrawn one among the untracked, for good.
func TestTrackingRetentionThresholdCountsOnlyDeliveredOpenTokens(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	day := tracking.EventRetentionDays
	var ten, eleven []recipient
	for i := 0; i < 10; i++ {
		ten = append(ten, tracked(day+1, 1, 0))
	}
	for i := 0; i < 11; i++ {
		eleven = append(eleven, tracked(day+1, 1, 0))
	}
	failed := seedSend(t, s, retentionTemplate, day+1, nil, ten...)
	withdrawn := seedSend(t, s, retentionTemplate, day+1, nil, eleven...)
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_queue SET status = 'failed', sent_at = NULL WHERE id = $1`, failed.rows[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithdrawTracking(ctx, eleven[0].sub.String()); err != nil {
		t.Fatal(err)
	}

	got, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.Counts.TotalsWritten != 2 {
		t.Fatalf("run = %s %+v, want ok and both sends' counts", got.Status, got.Counts)
	}
	if n := queryCount(t, s, `SELECT tracked_recipients FROM mail_task_tracking_totals WHERE task_id = $1`, failed.task); n != 9 {
		t.Errorf("nine delivered of ten tokens: counts of %d", n)
	}
	report, err := s.GetRetainedTrackingReport(ctx, withdrawn.task)
	if err != nil {
		t.Fatal(err)
	}
	var totals struct {
		Tracked    int            `json:"tracked_recipients"`
		Untracked  map[string]int `json:"untracked_by_reason"`
		UniqueOpen int            `json:"unique_opens"`
	}
	if err := json.Unmarshal(report, &totals); err != nil {
		t.Fatal(err)
	}
	if totals.Tracked != 10 || totals.UniqueOpen != 10 || !reflect.DeepEqual(totals.Untracked, map[string]int{"tracking_closed": 1}) {
		t.Errorf("totals = %+v, want 10 tracked, 10 opens, the withdrawn one tracking_closed", totals)
	}

	// A year on (and a day short of it): the counts of nine go, those of
	// ten stay.
	for _, age := range []string{"364 days", "366 days"} {
		if _, err := s.Conn.Exec(ctx, `UPDATE mail_tasks SET created_at = now() - $2::interval WHERE id = ANY($1)`,
			[]uuid.UUID{failed.task, withdrawn.task}, age); err != nil {
			t.Fatal(err)
		}
		got, err = s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply})
		if err != nil {
			t.Fatal(err)
		}
		wantDeleted, wantNine := int64(0), 1
		if age == "366 days" {
			wantDeleted, wantNine = 1, 0
		}
		if got.Counts.TotalsDeleted != wantDeleted {
			t.Errorf("%s: %+v, want %d counts deleted", age, got.Counts, wantDeleted)
		}
		if n := queryCount(t, s, `SELECT count(*) FROM mail_task_tracking_totals WHERE task_id = $1`, failed.task); n != wantNine {
			t.Errorf("%s: %d counts of nine, want %d", age, n, wantNine)
		}
		if n := queryCount(t, s, `SELECT count(*) FROM mail_task_tracking_totals WHERE task_id = $1`, withdrawn.task); n != 1 {
			t.Errorf("%s: the counts of ten went", age)
		}
	}
}

// A second run that finds, once it holds the lock, that another just ran
// does nothing and writes no receipt (OnlyIfDue; two replicas that both
// found the run due run it once).
func TestTrackingRetentionOnlyIfDueChecksUnderTheLock(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	if _, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionDryRun, OnlyIfDue: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionDryRun, OnlyIfDue: true}); !errors.Is(err, ErrTrackingRetentionNotDue) {
		t.Fatalf("second run: %v, want ErrTrackingRetentionNotDue", err)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM tracking_retention_runs`); n != 1 {
		t.Errorf("%d receipts, want 1", n)
	}
	if _, err := s.RunTrackingRetention(ctx, TrackingRetentionOptions{Mode: tracking.RetentionApply, OnlyIfDue: true}); err != nil {
		t.Errorf("apply after a dry run: %v", err)
	}
}

// A withdrawal does not wait on a held row for longer than its lock
// timeout: it fails with lock_not_available, which the route answers 503
// with Retry-After, and core tries again.
func TestWithdrawTrackingGivesUpOnAHeldRow(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	deniz := tracked(1, 1, 0)
	send := seedSend(t, s, retentionTemplate, 1, nil, deniz)
	tx, err := s.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT 1 FROM mail_queue WHERE id = $1 FOR UPDATE`, send.rows[0]); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	bounded, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	_, err = s.WithdrawTracking(bounded, deniz.sub.String())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("withdrawal on a held row: %v, want lock_not_available", err)
	}
	if waited := time.Since(start); waited > 8*time.Second {
		t.Errorf("waited %s", waited)
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id = $1`, send.rows[0]); n != 1 {
		t.Error("a failed withdrawal deleted events")
	}
}
