package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

func TestTrackingRecordingIsBoundedAndSerialized(t *testing.T) {
	s := erasureStore(t)
	ctx := context.Background()
	task := uuid.New()
	queue := uuid.New()
	token, _ := tracking.NewRecipientToken()
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tasks(id,sent_by,template_id,body_variables,tracking_requested) VALUES($1,'sender','10000000-0000-4000-8000-000000000001','{}',true)`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_queue(id,task_id,recipient_full_name,recipient_email,recipient_sub,subject,body,status,sent_at,tracking_token) VALUES($1,$2,'Member','member@example.invalid',$3,'x','x','sent',now()-interval '2 minutes',$4)`, queue, task, uuid.New(), token); err != nil {
		t.Fatal(err)
	}
	links := make([]int64, 60)
	for i := range links {
		lt, _ := tracking.NewLinkToken()
		if err := s.Conn.QueryRow(ctx, `INSERT INTO mail_task_links(task_id,link_token,url,position) VALUES($1,$2,$3,$4) RETURNING id`, task, lt, fmt.Sprintf("https://destination.invalid/%d", i), i).Scan(&links[i]); err != nil {
			t.Fatal(err)
		}
	}
	record := func(link int64) (bool, error) {
		return s.RecordTracking(ctx, token, link, "Mozilla/5.0 Chrome/120", "GET")
	}
	race := func(n int, fn func(int) error) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs <- fn(i) }(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
	}
	race(16, func(int) error { _, err := record(0); return err })
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1`, queue); n != 1 {
		t.Fatalf("concurrent open window = %d", n)
	}
	// Existing old observations exercise the SQL total cap independently of
	// the five-minute spacing window.
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_tracking_events SET occurred_at=now()-interval '2 hours' WHERE queue_id=$1`, queue); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 19; i++ {
		if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tracking_events(queue_id,task_id,kind,occurred_at,source_class,automated_likely,classifier_version) VALUES($1,$2,'open',now()-interval '2 hours','browser',false,1)`, queue, task); err != nil {
			t.Fatal(err)
		}
	}
	race(16, func(int) error { _, err := record(0); return err })
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1 AND kind='open'`, queue); n != 20 {
		t.Fatalf("open cap=%d", n)
	}
	race(16, func(int) error { _, err := record(links[0]); return err })
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1 AND kind='click'`, queue); n != 1 {
		t.Fatalf("same link minute cap=%d", n)
	}
	for i := 1; i < 3; i++ {
		if _, err := record(links[i]); err != nil {
			t.Fatal(err)
		}
	}
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1 AND kind='click' AND source_class='scanner_likely' AND automated_likely`, queue); n != 3 {
		t.Fatalf("whole scanner burst=%d", n)
	}
	race(57, func(i int) error { _, err := record(links[i+3]); return err })
	if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1 AND kind='click'`, queue); n != 50 {
		t.Fatalf("click cap=%d", n)
	}
	// Recipient/link mismatch never redirects tracking into another send.
	anotherTask := uuid.New()
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tasks(id,sent_by,template_id,body_variables) VALUES($1,'sender','10000000-0000-4000-8000-000000000001','{}')`, anotherTask); err != nil {
		t.Fatal(err)
	}
	lt, _ := tracking.NewLinkToken()
	var otherLink int64
	if err := s.Conn.QueryRow(ctx, `INSERT INTO mail_task_links(task_id,link_token,url,position) VALUES($1,$2,'https://other.invalid',0) RETURNING id`, anotherTask, lt).Scan(&otherLink); err != nil {
		t.Fatal(err)
	}
	if written, err := record(otherLink); err != nil || written {
		t.Errorf("mismatched link: %v %v", written, err)
	}
	// Clear observations to prove disabled, erased, pending and 90-day expiry
	// reject writes independently of the total caps.
	if _, err := s.Conn.Exec(ctx, `DELETE FROM mail_tracking_events WHERE queue_id=$1`, queue); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{
		`UPDATE mail_queue SET tracking_disabled_at=now() WHERE id=$1`,
		`UPDATE mail_queue SET tracking_disabled_at=NULL,recipient_sub=NULL WHERE id=$1`,
		`UPDATE mail_queue SET recipient_sub='b7d2e4f0-2a6c-4e8b-8f1d-5e6f7a8b9c0d',sent_at=now()-interval '90 days' WHERE id=$1`,
		`UPDATE mail_queue SET sent_at=now()-interval '89 days',status='pending' WHERE id=$1`,
	} {
		if _, err := s.Conn.Exec(ctx, change, queue); err != nil {
			t.Fatal(err)
		}
		if written, err := record(0); written || !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: %v %v", change, written, err)
		}
	}
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_queue SET status='sent',sent_at=now()-interval '89 days' WHERE id=$1`, queue); err != nil {
		t.Fatal(err)
	}
	if written, err := record(0); !written || err != nil {
		t.Fatalf("89 days: %v %v", written, err)
	}
	if _, err := s.Conn.Exec(ctx, `UPDATE mail_tasks SET tracking_closed_at=now() WHERE id=$1`, task); err != nil {
		t.Fatal(err)
	}
	if written, err := record(links[1]); written || !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("retention closed: %v %v", written, err)
	}
}

func TestTrackingReportAndRecipientFilters(t *testing.T) {
	s := erasureStore(t)
	ctx := context.Background()
	task := uuid.New()
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tasks(id,sent_by,template_id,body_variables,tracking_requested) VALUES($1,'sender','10000000-0000-4000-8000-000000000001','{}',true)`, task); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 4)
	for i := range ids {
		ids[i] = uuid.New()
		token, _ := tracking.NewRecipientToken()
		if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_queue(id,task_id,recipient_full_name,recipient_email,recipient_sub,subject,body,status,sent_at,tracking_token) VALUES($1,$2,$3,$4,$5,'x','x','sent',now()-interval '80 hours',$6)`, ids[i], task, fmt.Sprintf("Member%d", i), fmt.Sprintf("member%d@example.invalid", i), uuid.New(), token); err != nil {
			t.Fatal(err)
		}
	}
	lt, _ := tracking.NewLinkToken()
	var link int64
	if err := s.Conn.QueryRow(ctx, `INSERT INTO mail_task_links(task_id,link_token,url,position) VALUES($1,$2,'https://private.invalid/member0@example.invalid',0) RETURNING id`, task, lt).Scan(&link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tracking_events(queue_id,task_id,kind,link_id,occurred_at,source_class,automated_likely,classifier_version) VALUES
 ($1,$4,'open',NULL,now()-interval '79 hours','browser',false,1),
 ($2,$4,'open',NULL,now()-interval '78 hours','apple_proxy_likely',true,1),
 ($3,$4,'click',$5,now()-interval '1 hour','browser',false,1)`, ids[0], ids[1], ids[2], task, link); err != nil {
		t.Fatal(err)
	}
	report, err := s.GetTrackingReport(ctx, GetTrackingReportParams{TaskID: task, SentAt: time.Now().Add(-80 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var aggregate struct {
		Tracked int `json:"tracked_recipients"`
		Opens   int `json:"unique_opens"`
		Human   int `json:"unique_likely_human_opens"`
		Auto    int `json:"automated_only_recipients"`
		Series  []struct {
			Interval string `json:"interval"`
		} `json:"series"`
	}
	if err := json.Unmarshal(report, &aggregate); err != nil {
		t.Fatal(err)
	}
	if aggregate.Tracked != 4 || aggregate.Opens != 2 || aggregate.Human != 2 || aggregate.Auto != 1 || len(aggregate.Series) != 3 {
		t.Fatalf("aggregate: %s", report)
	}
	if aggregate.Series[0].Interval != "hour" || aggregate.Series[2].Interval != "day" {
		t.Fatalf("hour/day buckets: %s", report)
	}
	for filter, want := range map[string]int{"": 4, "opened": 3, "not_opened": 1, "clicked": 1, "automated_only": 1} {
		raw, err := s.ListTrackingRecipients(ctx, ListTrackingRecipientsParams{TaskID: task, Filter: filter, PageLimit: 2})
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Total int               `json:"total"`
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != want || len(page.Items) > 2 {
			t.Errorf("%s: %s", filter, raw)
		}
		if filter == "clicked" {
			var person map[string]any
			json.Unmarshal(page.Items[0], &person)
			if person["likely_human_open_source"] != "click_inferred" || person["first_likely_human_open_at"] == nil {
				t.Errorf("click inference: %s", raw)
			}
		}
	}
}

// A click writer holds queue -> link (the event's FK). Erasure must take
// every queue row it will update before clearing links, even when another
// recipient's rendered body names the person being erased.
func TestTrackingErasureSerializesBeforeClearingLinksAndEvents(t *testing.T) {
	for _, ownRecipient := range []bool{true, false} {
		t.Run(fmt.Sprintf("own_recipient_%t", ownRecipient), func(t *testing.T) {
			s := erasureStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			task, queue, sub := uuid.New(), uuid.New(), uuid.New()
			recipient := sub
			email, body := "concurrent@example.invalid", "x"
			if !ownRecipient {
				recipient = uuid.New()
				email = "other-recipient@example.invalid"
				body = "Contact concurrent@example.invalid"
			}
			token, _ := tracking.NewRecipientToken()
			linkToken, _ := tracking.NewLinkToken()
			if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_tasks(id,sent_by,template_id,body_variables,tracking_requested) VALUES($1,'sender','10000000-0000-4000-8000-000000000001','{}',true)`, task); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Conn.Exec(ctx, `INSERT INTO mail_queue(id,task_id,recipient_full_name,recipient_email,recipient_sub,subject,body,status,sent_at,tracking_token) VALUES($1,$2,'Concurrent Member',$3,$4,'x',$5,'sent',now()-interval '2 minutes',$6)`, queue, task, email, recipient, body, token); err != nil {
				t.Fatal(err)
			}
			var linkID int64
			if err := s.Conn.QueryRow(ctx, `INSERT INTO mail_task_links(task_id,link_token,url,position) VALUES($1,$2,'https://destination.invalid/concurrent%40example.invalid',0) RETURNING id`, task, linkToken).Scan(&linkID); err != nil {
				t.Fatal(err)
			}
			tx, err := s.Conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			q := s.WithTx(tx)
			locked, err := q.LockTrackingRecipient(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			erased := make(chan error, 1)
			go func() {
				_, err := s.EraseAccount(ctx, AccountErasure{RequestID: uuid.New(), Subject: sub.String(), Emails: []string{"concurrent@example.invalid"}})
				erased <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				blocked := queryCount(t, s, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%mail_queue q%'`)
				if blocked > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("erasure did not wait for the active click writer")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A bounded event context catches the lock inversion without waiting for
			// deadlock_timeout: the old erasure order already owns this link's key.
			clickCtx, stopClick := context.WithTimeout(ctx, 500*time.Millisecond)
			count, err := q.InsertTrackingEvent(clickCtx, InsertTrackingEventParams{QueueID: queue, Kind: "click", LinkID: pgtype.Int8{Int64: linkID, Valid: true}, At: locked.At, SourceClass: "browser", ClassifierVersion: 1})
			stopClick()
			if err != nil || count != 1 {
				_ = tx.Rollback(context.Background())
				select {
				case <-erased:
				case <-ctx.Done():
				}
				t.Fatalf("in-flight click/FK must not wait behind erasure's link lock: %d %v", count, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-erased:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("erasure did not finish")
			}
			want := 0
			if !ownRecipient {
				want = 1
			}
			if n := queryCount(t, s, `SELECT count(*) FROM mail_tracking_events WHERE queue_id=$1`, queue); n != want {
				t.Errorf("tracking events after erasure=%d, want %d", n, want)
			}
			if n := queryCount(t, s, `SELECT count(*) FROM mail_task_links WHERE id=$1 AND url IS NULL AND url_cleared_reason='erased'`, linkID); n != 1 {
				t.Error("personal link was not cleared")
			}
			if ownRecipient {
				if written, err := s.RecordTracking(ctx, token, linkID, "Mozilla/5.0 Chrome/120", "GET"); written || !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("after erasure: %v %v", written, err)
				}
			} else {
				if got := queryString(t, s, `SELECT body FROM mail_queue WHERE id=$1`, queue); got == nil || *got != "" {
					t.Error("other recipient's body still names the erased person")
				}
				if written, err := s.RecordTracking(ctx, token, linkID, "Mozilla/5.0 Chrome/120", "GET"); written || err != nil {
					t.Errorf("cleared link event guard: %v %v", written, err)
				}
			}
		})
	}
}
