package migrations_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migrations of open and click tracking (hub ticket iletisim-servisi 14):
// the columns on sends, requests and queue rows, the links, the events and
// the totals, then the queue's and the sends' indexes, one per file.
const (
	openClickTracking         = uint(20261015120000)
	mailQueueTrackingToken    = uint(20261015120100)
	mailQueueTrackingSubject  = uint(20261015120200)
	mailTasksTrackingOpenSend = uint(20261015120300)
)

// What the image from before tracking (#67) sends the database, as its sqlc
// generated it: it names its columns, so it neither reads nor writes the new
// ones.
const (
	channelImageTaskInsert = `
		INSERT INTO mail_tasks (id, sent_by, body_variables, content_sensitive_at_send)
		VALUES ('40000000-0000-4000-8000-000000000014', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}', 'false')`
	channelImageQueueInsert = `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, priority, recipient_sub)
		VALUES ('40000000-0000-4000-8000-000000000014', 'Eski', 'eski@example.com', 'Konu', 'Gövde', '<p>x</p>', 0,
		        '7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10')`
	channelImageBulkClaim = `
		UPDATE mail_queue
		SET status     = 'processing',
		    claimed_at = NOW(),
		    claimed_by = $1::text
		WHERE id IN (SELECT id
		             FROM mail_queue
		             WHERE status = 'pending'
		               AND priority <= 0
		               AND next_attempt_at <= NOW()
		             ORDER BY next_attempt_at, created_at
		             LIMIT $2::int FOR UPDATE SKIP LOCKED)
		RETURNING id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status, error, created_at, attempts, next_attempt_at, claimed_at, claimed_by, priority, channel, recipient_sub`
	channelImageSent = `
		UPDATE mail_queue
		SET status    = 'sent',
		    error     = NULL
		WHERE status = 'processing'
		  AND claimed_by = $1::text`
)

// Up adds everything tracking keeps and none of what it must not: a row an
// image from before it queues, takes and sends is untracked (no token, no
// reason, no sent time); the events hold no address, client string or token;
// the checks refuse what tracking never writes. Down drops it all and leaves
// the queue's rows.
func TestOpenClickTrackingMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailQueueChannel); err != nil {
		t.Fatalf("migrate to %d: %v", mailQueueChannel, err)
	}
	if _, err := database.Pool.Exec(ctx, channelImageTaskInsert); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, channelImageQueueInsert); err != nil {
		t.Fatal(err)
	}

	if err := runner.Migrate(mailTasksTrackingOpenSend); err != nil {
		t.Fatalf("up: %v", err)
	}

	// The image from before goes on as it did.
	if _, err := database.Pool.Exec(ctx, channelImageQueueInsert); err != nil {
		t.Fatalf("an old image's insert: %v", err)
	}
	rows, err := database.Pool.Query(ctx, channelImageBulkClaim, "old-image", 10)
	if err != nil {
		t.Fatalf("an old image's claim: %v", err)
	}
	claimed := 0
	for rows.Next() {
		claimed++
	}
	rows.Close()
	if err := rows.Err(); err != nil || claimed != 2 {
		t.Fatalf("an old image claimed %d rows (%v), want both", claimed, err)
	}
	if tag, err := database.Pool.Exec(ctx, channelImageSent, "old-image"); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("an old image's sent: %v, %d rows", err, tag.RowsAffected())
	}
	var untracked, total int
	var requested bool
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE tracking_token IS NULL AND tracking_skip_reason IS NULL
		                         AND tracking_disabled_at IS NULL AND sent_at IS NULL),
		       count(*),
		       (SELECT tracking_requested FROM mail_tasks WHERE id = '40000000-0000-4000-8000-000000000014')
		FROM mail_queue`).Scan(&untracked, &total, &requested); err != nil {
		t.Fatal(err)
	}
	if total != 2 || untracked != 2 || requested {
		t.Fatalf("after up: %d of %d rows untracked, send tracking_requested %v; want every row untracked and the send not",
			untracked, total, requested)
	}

	// The events hold the kind, the link, when and the client's class: no
	// address, no client string, no referrer, no token.
	columnsOf := func(table string) []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			SELECT column_name FROM information_schema.columns WHERE table_name = $1 ORDER BY column_name`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		return names
	}
	wantColumns := map[string][]string{
		"mail_tracking_events": {"automated_likely", "classifier_version", "id", "kind", "link_id", "occurred_at",
			"queue_id", "source_class", "task_id"},
		"mail_task_tracking_totals": {"clicks_by_link", "rolled_up_at", "task_id", "tracked_recipients",
			"unique_clickers", "unique_likely_human_opens", "unique_opens", "untracked_by_reason"},
		"mail_task_links": {"created_at", "id", "link_token", "position", "task_id", "url", "url_cleared_reason"},
	}
	for table, want := range wantColumns {
		if got := columnsOf(table); !slices.Equal(got, want) {
			t.Errorf("%s columns = %v, want exactly %v", table, got, want)
		}
	}

	exec := func(query string, args ...any) error {
		_, err := database.Pool.Exec(ctx, query, args...)
		return err
	}
	violates := func(what, code string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: %v, want SQLSTATE %s", what, err, code)
		}
	}
	const queueRow = `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, tracking_token, tracking_skip_reason)
		VALUES ('40000000-0000-4000-8000-000000000014', 'Yeni', 'yeni@example.com', 'Konu', 'Gövde', $1, $2)`
	token := make([]byte, 16)
	token[0] = 1
	if err := exec(queueRow, token, nil); err != nil {
		t.Fatalf("a tracked row: %v", err)
	}
	violates("the same token twice", "23505", exec(queueRow, token, nil))
	violates("a token of 15 bytes", "23514", exec(queueRow, make([]byte, 15), nil))
	for _, reason := range []string{"flag_off", "transactional", "no_account", "preference_off", "not_member",
		"inactive", "preference_unavailable", "not_allowed"} {
		if err := exec(queueRow, nil, reason); err != nil {
			t.Errorf("skip reason %q: %v", reason, err)
		}
	}
	violates("an unknown skip reason", "23514", exec(queueRow, nil, "bogus"))

	var queueID string
	if err := database.Pool.QueryRow(ctx, `SELECT id::text FROM mail_queue WHERE tracking_token IS NOT NULL`).Scan(&queueID); err != nil {
		t.Fatal(err)
	}
	const link = `
		INSERT INTO mail_task_links (task_id, link_token, url, position, url_cleared_reason)
		VALUES ('40000000-0000-4000-8000-000000000014', $1, $2, $3, $4)`
	if err := exec(link, []byte("12345678"), "https://example.com/a", 0, nil); err != nil {
		t.Fatalf("a link: %v", err)
	}
	violates("a link token of 7 bytes", "23514", exec(link, []byte("1234567"), "https://example.com/b", 1, nil))
	violates("a link token twice", "23505", exec(link, []byte("12345678"), "https://example.com/b", 1, nil))
	violates("an address twice in a send", "23505", exec(link, []byte("87654321"), "https://example.com/a", 1, nil))
	violates("a position twice in a send", "23505", exec(link, []byte("87654321"), "https://example.com/b", 0, nil))
	violates("an address longer than 2048", "23514", exec(link, []byte("87654321"), "https://example.com/"+strings.Repeat("a", 2030), 1, nil))
	violates("an emptied address without why", "23514", exec(link, []byte("87654321"), nil, 1, nil))
	violates("an address with why it was emptied", "23514", exec(link, []byte("87654321"), "https://example.com/b", 1, "expired"))
	for i, reason := range []string{"expired", "erased"} {
		if err := exec(link, []byte{'e', 'm', 'p', 't', 'i', 'e', 'd', byte(i)}, nil, 2+i, reason); err != nil {
			t.Errorf("an address emptied, %s: %v", reason, err)
		}
	}

	const event = `
		INSERT INTO mail_tracking_events (queue_id, task_id, kind, link_id, source_class, automated_likely, classifier_version)
		VALUES ($1, '40000000-0000-4000-8000-000000000014', $2, $3, $4, false, 1)`
	var linkID int64
	if err := database.Pool.QueryRow(ctx, `SELECT id FROM mail_task_links WHERE position = 0`).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	if err := exec(event, queueID, "open", nil, "browser"); err != nil {
		t.Fatalf("an opening: %v", err)
	}
	if err := exec(event, queueID, "click", linkID, "apple_proxy_likely"); err != nil {
		t.Fatalf("a click: %v", err)
	}
	violates("a click without its link", "23514", exec(event, queueID, "click", nil, "browser"))
	violates("an opening with a link", "23514", exec(event, queueID, "open", linkID, "browser"))
	violates("an unknown kind", "23514", exec(event, queueID, "forward", nil, "browser"))
	violates("an unknown class", "23514", exec(event, queueID, "open", nil, "Mozilla/5.0"))

	const totals = `
		INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers)
		VALUES ('40000000-0000-4000-8000-000000000014', $1, 0, 0, 0)`
	violates("totals of fewer than 10 tracked people", "23514", exec(totals, 9))
	if err := exec(totals, 10); err != nil {
		t.Errorf("totals of 10 tracked people: %v", err)
	}

	var valid int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_index
		WHERE indisvalid
		  AND indexrelid::regclass::text IN ('idx_mail_queue_tracking_token', 'idx_mail_queue_tracking_subject',
		                                     'idx_mail_tasks_tracking_open')`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 3 {
		t.Errorf("%d of the 3 tracking indexes are built", valid)
	}

	if err := runner.Migrate(mailQueueChannel); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns, tables, indexes, left int
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		        WHERE (table_name = 'mail_queue' AND column_name IN ('tracking_token', 'tracking_disabled_at',
		                                                             'tracking_skip_reason', 'sent_at'))
		           OR (table_name = 'mail_tasks' AND column_name IN ('tracking_requested', 'tracking_closed_at'))
		           OR (table_name = 'mail_approvals' AND column_name = 'tracking_requested')),
		       (SELECT count(*) FROM information_schema.tables
		        WHERE table_name IN ('mail_task_links', 'mail_tracking_events', 'mail_task_tracking_totals')),
		       (SELECT count(*) FROM pg_class
		        WHERE relname IN ('idx_mail_queue_tracking_token', 'idx_mail_queue_tracking_subject',
		                          'idx_mail_tasks_tracking_open')),
		       (SELECT count(*) FROM mail_queue)`).Scan(&columns, &tables, &indexes, &left); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || tables != 0 || indexes != 0 || left != 11 {
		t.Fatalf("after down: %d columns, %d tables, %d indexes, %d rows; want 0, 0, 0 and 11", columns, tables, indexes, left)
	}
}
