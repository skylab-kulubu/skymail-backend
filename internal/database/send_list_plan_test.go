package database

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The send list's queries over 50,000 sends (hub ticket skymail-yeni-tasarim
// 16, review): 48,000 single mails of one recipient each and 2,000 tracked
// list sends of 20, 88,000 queue rows. Each plan's execution time is logged
// (go test -v), for the pull request; nothing is asserted about it, a CI
// runner's speed varies.
func TestSendListPlansOverFiftyThousandSends(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	started := time.Now()
	for _, sql := range []string{
		`INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, system) VALUES
		    ('66000000-0000-4000-8000-000000000001', 'panel.duyuru', 'Duyuru şablonu', 'Bahar şenliği', '<p>x</p>', 'x', '', false),
		    ('66000000-0000-4000-8000-000000000002', 'keycloak.login-code', 'Giriş kodu', 'Kodunuz', '<p>x</p>', 'x', '', true)`,
		`INSERT INTO mailing_lists (id, name) VALUES ('67000000-0000-4000-8000-000000000001', 'Tüm üyeler')`,
		`INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, created_at, tracking_requested, sent_by_service)
		 SELECT md5('t' || n)::uuid, 'sub-' || (n % 40),
		        CASE WHEN n % 25 = 0 OR n % 5 = 0 THEN '66000000-0000-4000-8000-000000000001'::uuid
		             ELSE '66000000-0000-4000-8000-000000000002'::uuid END,
		        CASE WHEN n % 25 = 0 THEN '67000000-0000-4000-8000-000000000001'::uuid END,
		        '{}', now() - n * interval '5 minutes', n % 25 = 0,
		        CASE WHEN n % 25 = 0 THEN NULL ELSE n % 5 <> 0 END
		 FROM generate_series(1, 50000) AS n`,
		`INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority, sent_at)
		 SELECT md5('t' || n)::uuid, 'Kişi ' || n, 'kisi' || n || '@ornek.example.net', 'K', 'G', 'sent', 1,
		        now() - n * interval '5 minutes'
		 FROM generate_series(1, 50000) AS n
		 WHERE n % 25 <> 0`,
		`INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority, sent_at, tracking_token)
		 SELECT md5('t' || n)::uuid, 'Üye ' || k, 'uye' || k || '-' || n || '@ornek.example.net', 'K', 'G', 'sent', 0,
		        now() - n * interval '5 minutes', decode(md5(n || '-' || k), 'hex')
		 FROM generate_series(25, 50000, 25) AS n, generate_series(1, 20) AS k`,
		`INSERT INTO mail_tracking_events (queue_id, task_id, kind, source_class, automated_likely, classifier_version)
		 SELECT q.id, q.task_id, 'open', 'mail_client', false, 1
		 FROM mail_queue q
		 WHERE q.priority = 0 AND q.recipient_email ~ '^uye[1-5]-'`,
		`ANALYZE`,
	} {
		if _, err := db.Conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	t.Logf("seeded in %s", time.Since(started).Round(time.Millisecond))

	search := "kisi49999"
	// The queries' arguments, in the order sqlc binds them.
	list := func(search *string) []any {
		// limit, offset, task_id, status, search, created_from,
		// created_before, kind, template_id, list_id, sent_by.
		return []any{int32(20), int32(0), nil, nil, search, nil, nil, nil, nil, nil, nil}
	}
	sorted := func(sort string) []any {
		// limit, offset, ascending, task_id, status, search, created_from,
		// created_before, kind, template_id, list_id, sent_by, sort.
		return []any{int32(20), int32(0), false, nil, nil, nil, nil, nil, nil, nil, nil, nil, sort}
	}
	count := func(search *string) []any { return []any{nil, search, nil, nil, nil, nil, nil, nil} }
	executionTime := regexp.MustCompile(`Execution Time: ([0-9.]+ ms)`)
	for _, c := range []struct {
		name, query string
		args        []any
		generic     bool
		// A plan's node the query must use: the default page is read down
		// the index, never by reading every send.
		uses string
	}{
		{"default page", listMailTaskSends, list(nil), false, "idx_mail_tasks_created_at_id"},
		{"default page, generic plan", listMailTaskSends, list(nil), true, "idx_mail_tasks_created_at_id"},
		{"q (a recipient's address)", listMailTaskSends, list(&search), false, ""},
		{"q count", countMailTaskSends, count(&search), false, ""},
		{"count, no filter", countMailTaskSends, count(nil), false, ""},
		{"_sort=open_rate", listMailTaskSendsSorted, sorted("open_rate"), false, ""},
		{"_sort=open_rate, generic plan", listMailTaskSendsSorted, sorted("open_rate"), true, ""},
		{"_order=asc", listMailTaskSendsSorted, append(sorted("")[:2:2], append([]any{true}, sorted("")[3:]...)...), false, ""},
	} {
		conn, err := db.Conn.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mode := "auto"
		if c.generic {
			mode = "force_generic_plan"
		}
		if _, err := conn.Exec(ctx, `SET plan_cache_mode = `+mode); err != nil {
			t.Fatal(err)
		}
		rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+c.query, c.args...)
		if err != nil {
			conn.Release()
			t.Fatalf("%s: %v", c.name, err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		_, _ = conn.Exec(ctx, `RESET plan_cache_mode`)
		conn.Release()
		if rows.Err() != nil {
			t.Fatalf("%s: %v", c.name, rows.Err())
		}
		joined := strings.Join(plan, "\n")
		took := "?"
		if m := executionTime.FindStringSubmatch(joined); m != nil {
			took = m[1]
		}
		t.Logf("PLAN %s: %s\n%s", c.name, took, joined)
		if c.uses != "" && !strings.Contains(joined, c.uses) {
			t.Errorf("%s does not use %s:\n%s", c.name, c.uses, joined)
		}
	}
}
