package database

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// Open and click tracking under Account erasure (hub ticket iletisim-servisi
// 14). Deniz and Ayşe got a tracked announcement; Deniz alone got a tracked
// send too. Deniz's openings and clicks go and the tokens of mail to Deniz
// close; Ayşe's opening stays and her token stays open. The links whose
// address names Deniz — as written, with the @ encoded, or the full name with
// its blank as + and its ı as %C4%B1 — and the link of the send to Deniz
// alone are emptied; the announcement's shared link stays.
func TestEraseAccountTakesThePersonOutOfTracking(t *testing.T) {
	store := erasureStore(t)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, tracking_requested) VALUES
		    ('40000000-0000-4000-8000-00000000f001', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}', true),
		    ('40000000-0000-4000-8000-00000000f002', '`+otherSub+`', '10000000-0000-4000-8000-000000000001', '{}', true);
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, recipient_sub, tracking_token) VALUES
		    ('50000000-0000-4000-8000-00000000f001', '40000000-0000-4000-8000-00000000f001', 'Deniz Yılmaz', 'deniz@example.com', 'Duyuru', 'x', 'sent', '`+erasedSub+`', '\x00000000000000000000000000000001'),
		    ('50000000-0000-4000-8000-00000000f002', '40000000-0000-4000-8000-00000000f001', 'Ayşe Kaya', 'ayse@example.com', 'Duyuru', 'x', 'sent', '`+otherSub+`', '\x00000000000000000000000000000002'),
		    ('50000000-0000-4000-8000-00000000f003', '40000000-0000-4000-8000-00000000f002', 'Deniz Yılmaz', 'eski@example.org', 'Özel', 'x', 'sent', '`+erasedSub+`', '\x00000000000000000000000000000003');
		INSERT INTO mail_task_links (id, task_id, link_token, url, position) OVERRIDING SYSTEM VALUE VALUES
		    (1, '40000000-0000-4000-8000-00000000f001', '\x0000000000000001', 'https://skylab.example/etkinlik', 0),
		    (2, '40000000-0000-4000-8000-00000000f001', '\x0000000000000002', 'https://skylab.example/u?e=DENIZ%40example.com', 1),
		    (3, '40000000-0000-4000-8000-00000000f001', '\x0000000000000003', 'https://skylab.example/k/deniz.yilmaz@std.yildiz.edu.tr', 2),
		    (4, '40000000-0000-4000-8000-00000000f001', '\x0000000000000004', 'https://skylab.example/ad/Deniz+Y%C4%B1lmaz', 3),
		    (5, '40000000-0000-4000-8000-00000000f002', '\x0000000000000005', 'https://skylab.example/ozel', 0);
		INSERT INTO mail_tracking_events (queue_id, task_id, kind, link_id, source_class, automated_likely, classifier_version) VALUES
		    ('50000000-0000-4000-8000-00000000f001', '40000000-0000-4000-8000-00000000f001', 'open', NULL, 'browser', false, 1),
		    ('50000000-0000-4000-8000-00000000f001', '40000000-0000-4000-8000-00000000f001', 'click', 1, 'browser', false, 1),
		    ('50000000-0000-4000-8000-00000000f003', '40000000-0000-4000-8000-00000000f002', 'open', NULL, 'apple_proxy_likely', true, 1),
		    ('50000000-0000-4000-8000-00000000f002', '40000000-0000-4000-8000-00000000f001', 'open', NULL, 'google_proxy', false, 1);`); err != nil {
		t.Fatal(err)
	}

	requestID := uuid.New()
	receipt, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if err != nil {
		t.Fatal(err)
	}
	counts := receiptCounts(t, receipt)
	if counts[AccountErasureTrackingEventsDeleted] != 3 || counts[AccountErasureTrackingLinksCleared] != 4 {
		t.Errorf("counts = %v, want 3 tracking events deleted and 4 links cleared", counts)
	}

	if n := queryCount(t, store, `SELECT count(*) FROM mail_tracking_events`); n != 1 {
		t.Errorf("%d tracking events left, want Ayşe's one", n)
	}
	if n := queryCount(t, store, `SELECT count(*) FROM mail_tracking_events WHERE queue_id = '50000000-0000-4000-8000-00000000f002'`); n != 1 {
		t.Error("Ayşe's opening went")
	}
	if n := queryCount(t, store, `
		SELECT count(*) FROM mail_queue
		WHERE id IN ('50000000-0000-4000-8000-00000000f001', '50000000-0000-4000-8000-00000000f003')
		  AND tracking_disabled_at IS NOT NULL AND tracking_token IS NULL AND recipient_sub IS NULL`); n != 2 {
		t.Errorf("%d of the 2 rows to Deniz have their token dropped", n)
	}
	if got := queryString(t, store, `SELECT tracking_disabled_at::text FROM mail_queue WHERE id = '50000000-0000-4000-8000-00000000f002'`); got != nil {
		t.Errorf("Ayşe's token closed at %s", *got)
	}

	links := map[int]string{}
	rows, err := store.Conn.Query(ctx, `SELECT id, coalesce(url, '') || '|' || coalesce(url_cleared_reason, '') FROM mail_task_links`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		links[id] = state
	}
	rows.Close()
	want := map[int]string{
		1: "https://skylab.example/etkinlik|",
		2: "|erased",
		3: "|erased",
		4: "|erased",
		// The send went to Deniz alone.
		5: "|erased",
	}
	if !reflect.DeepEqual(links, want) {
		t.Errorf("links = %v\nwant    %v", links, want)
	}

	again, err := store.EraseAccount(ctx, eraseDeniz(requestID))
	if err != nil || !reflect.DeepEqual(again, receipt) {
		t.Errorf("repeat = %+v, %v; want the first receipt", again, err)
	}
}

// The names searched for come from Postgres's lower(), which leaves Turkish
// capitals as they are under the C ctype; the address is lower-cased in Go.
// The needle is lower-cased in Go too, so both sides fold the same way
// whatever the database's ctype.
func TestLinkNamesFoldsTheNeedleAsWellAsTheLink(t *testing.T) {
	for _, c := range []struct {
		link   string
		needle string
		want   bool
	}{
		{"https://skylab.example/ad/%C4%B0smail+%C5%9Eahin", "İsmail Şahin", true},
		{"https://skylab.example/u?e=deniz%40example.com", "DENIZ@EXAMPLE.COM", true},
		{"https://skylab.example/ad/Ayse+Kaya", "İsmail Şahin", false},
		{"https://skylab.example/", "", false},
	} {
		if got := linkNames(c.link, []string{c.needle}); got != c.want {
			t.Errorf("linkNames(%q, %q) = %v, want %v", c.link, c.needle, got, c.want)
		}
	}
}
