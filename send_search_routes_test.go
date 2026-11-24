package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const (
	searchOperator = "31ef736f-72da-4a40-8791-d523199cf9f0"
	searchOther    = "8a3c1d52-0b7e-4c41-9f0e-2d6a8e5b7c90"

	searchPanelTemplate  = "61000000-0000-4000-8000-000000000001"
	searchSystemTemplate = "61000000-0000-4000-8000-000000000002"
	searchList           = "62000000-0000-4000-8000-000000000001"

	// A: Tüm üyeler, 12 tracked, 3 likely opened (0.25). B: Ayşe alone,
	// tracked and opened (1.0). C: a login code (System template). D: two
	// people, one cancelled (skipped) and one refused. E: an approved send,
	// its tracking rolled up (5 of 10, 0.5).
	searchSendA = "63000000-0000-4000-8000-00000000000a"
	searchSendB = "63000000-0000-4000-8000-00000000000b"
	searchSendC = "63000000-0000-4000-8000-00000000000c"
	searchSendD = "63000000-0000-4000-8000-00000000000d"
	searchSendE = "63000000-0000-4000-8000-00000000000e"
)

// searchApp serves the send list, a send's recipients and the templates over
// five sends; the caller's roles are the X-Test-Roles header, comma
// separated.
func searchApp(t *testing.T) (*fiber.App, *database.Store) {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content, system) VALUES
		    ('` + searchPanelTemplate + `', 'panel.duyuru', 'Duyuru şablonu', 'Bahar şenliği', '<p>x</p>', 'x', '', false),
		    ('` + searchSystemTemplate + `', 'keycloak.login-code', 'Giriş kodu', 'Kodunuz', '<p>x</p>', 'x', '', true)`,
		`INSERT INTO mailing_lists (id, name, description) VALUES ('` + searchList + `', 'Tüm üyeler', 'Kulübün bütün üyeleri')`,
		`INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, created_at) VALUES
		    ('` + searchSendA + `', '` + searchOperator + `', '` + searchPanelTemplate + `', '` + searchList + `', '{}', '2026-11-01 10:00+03'),
		    ('` + searchSendB + `', '` + searchOther + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-10 10:00+03'),
		    ('` + searchSendC + `', 'keycloak-service', '` + searchSystemTemplate + `', NULL, '{}', '2026-11-20 10:00+03'),
		    ('` + searchSendD + `', '` + searchOperator + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-15 23:30+03'),
		    ('` + searchSendE + `', '` + searchOperator + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-05 10:00+03')`,
		// A: 12 list recipients in the bulk lane, tracked, sent yesterday.
		`INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority, sent_at, tracking_token)
		 SELECT '` + searchSendA + `', 'Üye ' || n, 'uye' || n || '@ornek.example.net', 'Bahar şenliği', 'G', 'sent', 0,
		        now() - interval '1 day', decode(md5(n::text), 'hex')
		 FROM generate_series(1, 12) AS n`,
		`INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, priority, sent_at, tracking_token) VALUES
		    ('` + searchSendB + `', 'Ayşe Yılmaz', 'ayse@ornek.example.net', 'Bahar şenliği', 'G', 'sent', 1, now() - interval '1 day', decode(md5('b'), 'hex')),
		    ('` + searchSendC + `', 'Mehmet Kaya', 'mehmet@ornek.example.net', 'Kodunuz', 'G', 'sent', 1, now() - interval '1 day', NULL)`,
		`INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error, priority) VALUES
		    ('` + searchSendD + `', 'Zeynep Ak', 'zeynep@ornek.example.net', '', '', 'failed', 'cancelled', 0),
		    ('` + searchSendD + `', 'Ali Can', 'ali@uzak.example.net', '', '', 'failed', 'smtp: RCPT TO 550 5.1.1', 0),
		    ('` + searchSendE + `', 'Ece Su', 'ece@ornek.example.net', 'Bahar şenliği', 'G', 'sent', NULL, 0)`,
		// Three of A's people likely opened; a fourth only through a proxy.
		`INSERT INTO mail_tracking_events (queue_id, task_id, kind, source_class, automated_likely, classifier_version)
		 SELECT q.id, q.task_id, 'open', CASE WHEN q.recipient_email LIKE 'uye4@%' THEN 'apple_proxy_likely' ELSE 'mail_client' END,
		        q.recipient_email LIKE 'uye4@%', 1
		 FROM mail_queue q
		 WHERE q.task_id = '` + searchSendA + `' AND q.recipient_email ~ '^uye[1-4]@'`,
		`INSERT INTO mail_tracking_events (queue_id, task_id, kind, source_class, automated_likely, classifier_version)
		 SELECT q.id, q.task_id, 'open', 'mail_client', false, 1 FROM mail_queue q WHERE q.task_id = '` + searchSendB + `'`,
		`UPDATE mail_tasks SET tracking_closed_at = now() WHERE id = '` + searchSendE + `'`,
		`INSERT INTO mail_task_tracking_totals (task_id, tracked_recipients, unique_opens, unique_likely_human_opens, unique_clickers)
		 VALUES ('` + searchSendE + `', 10, 6, 5, 1)`,
	} {
		if _, err := postgres.Pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	// E is an approved send: Fatma Demir's request. Its version and people
	// are left out, so the deferred checks are off for the insert.
	tx, err := postgres.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, sql := range []string{
		`SET LOCAL session_replication_role = replica`,
		`WITH approval AS (
		     INSERT INTO mail_approvals (submitter_sub, submitter_name, template_id, template_version_id, body_variables,
		                                 created_at, submitted_at, deadline_at, updated_at, state)
		     VALUES ('` + searchOperator + `', 'Fatma Demir', '` + searchPanelTemplate + `', gen_random_uuid(), '{}',
		             now(), now(), now() + interval '7 days', now(), 'approved')
		     RETURNING id)
		 INSERT INTO mail_approval_tasks (approval_id, position, task_id) SELECT id, 1, '` + searchSendE + `' FROM approval`,
	} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", searchOperator)
		c.Locals("roles", strings.Split(c.Get("X-Test-Roles"), ","))
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	registerTemplateRoutes(api, auth, handlers.NewTemplateHandler(store))
	return app, store
}

const (
	searchReader  = "skymail:mails:read,skymail:templates:read"
	searchTracker = searchReader + "," + handlers.TrackingReadRole
)

func searchGet(t *testing.T, app *fiber.App, roles, target string) (int, string, string) {
	t.Helper()
	request := httptest.NewRequest("GET", target, nil)
	request.Header.Set("X-Test-Roles", roles)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body), response.Header.Get("X-Total-Count")
}

type searchedSend struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func letters(t *testing.T, raw string) string {
	t.Helper()
	var sends []searchedSend
	if err := json.Unmarshal([]byte(raw), &sends); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var out strings.Builder
	for _, s := range sends {
		out.WriteString(strings.ToUpper(s.ID[len(s.ID)-1:]))
	}
	return out.String()
}

func TestSendListSearchFiltersAndSorts(t *testing.T) {
	app, _ := searchApp(t)

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "CDBEA"},
		{"?q=%C3%BCyeler", "A"},     // the list's name
		{"?q=PANEL.DUYURU", "DBEA"}, // the template's key, any case
		{"?q=bahar", "DBEA"},        // the template's subject
		{"?q=giri%C5%9F", "C"},      // the System template's name
		{"?q=ayse%40ornek", "B"},    // a recipient of a send to people
		{"?q=zeynep", "D"},          // the same, on a send that failed
		{"?q=uye1", ""},             // a list's recipients are not searched
		{"?q=fatma", "E"},           // an approved send's requester
		{"?kind=bulk", "EA"},        // a list, an approved send
		{"?kind=system", "C"},       // the System template's mail
		{"?kind=single", "DB"},      // any other
		{"?template_id=" + searchSystemTemplate, "C"},
		{"?list_id=" + searchList, "A"},
		{"?sent_by=" + searchOther, "B"},
		{"?from=2026-11-10&to=2026-11-15", "DB"}, // to takes the whole day
		{"?from=2026-11-10T10:00:00%2B03:00&to=2026-11-10T10:00:00%2B03:00", "B"},
		{"?_order=asc", "AEBDC"},
		{"?kind=single&q=bahar&_order=asc", "BD"},
	} {
		status, raw, total := searchGet(t, app, searchReader, "/v1/mail_tasks"+tc.query+sep(tc.query)+"_end=50")
		if status != 200 {
			t.Fatalf("%s: %d %s", tc.query, status, raw)
		}
		if got := letters(t, raw); got != tc.want || total != itoa(len(tc.want)) {
			t.Errorf("%s: sends %q, X-Total-Count %s; want %q, %d", tc.query, got, total, tc.want, len(tc.want))
		}
	}

	// Each send says its kind.
	_, raw, _ := searchGet(t, app, searchReader, "/v1/mail_tasks?_end=50")
	var sends []searchedSend
	if err := json.Unmarshal([]byte(raw), &sends); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, s := range sends {
		kinds[strings.ToUpper(s.ID[len(s.ID)-1:])] = s.Kind
	}
	if want := map[string]string{"A": "bulk", "B": "single", "C": "system", "D": "single", "E": "bulk"}; !equalMaps(kinds, want) {
		t.Errorf("kinds %v; want %v", kinds, want)
	}

	// The open rate order tells the counts GET /:id/tracking may withhold:
	// only the tracking role sorts by it. Sends with no rate come last.
	if status, raw, _ := searchGet(t, app, searchReader, "/v1/mail_tasks?_sort=open_rate"); status != 403 || !strings.Contains(raw, "mail_task.sort_forbidden") {
		t.Errorf("open rate without the role: %d %s", status, raw)
	}
	for query, want := range map[string]string{
		"?_sort=open_rate":                      "BEACD", // 1.0, 0.5 (rolled up), 0.25, then newest first
		"?_sort=open_rate&_order=asc":           "AEBDC",
		"?_sort=open_rate&_end=2":               "BE",
		"?_sort=open_rate&_start=2&_end=4":      "AC",
		"?_sort=open_rate&kind=bulk&_order=asc": "AE",
	} {
		status, raw, _ := searchGet(t, app, searchTracker, "/v1/mail_tasks"+query+sep(query)+"x=1")
		if status != 200 {
			t.Fatalf("%s: %d %s", query, status, raw)
		}
		if got := letters(t, raw); got != want {
			t.Errorf("%s: %q; want %q", query, got, want)
		}
	}

	for _, query := range []string{"?kind=list", "?from=yesterday", "?to=2026-13-01", "?template_id=x", "?list_id=1",
		"?_sort=subject", "?_order=up", "?from=2026-11-20&to=2026-11-10", "?q=" + strings.Repeat("a", 201)} {
		if status, raw, _ := searchGet(t, app, searchTracker, "/v1/mail_tasks"+query); status != 400 || !strings.Contains(raw, "validation.error") {
			t.Errorf("%s: %d %s; want 400 validation.error", query, status, raw)
		}
	}
	if status, _, _ := searchGet(t, app, "skymail:templates:read", "/v1/mail_tasks?q=bahar"); status != 403 {
		t.Errorf("without mails:read: %d", status)
	}
}

func TestSendRecipientsSearchAndSkipped(t *testing.T) {
	app, _ := searchApp(t)
	base := "/v1/mail_tasks/" + searchSendD + "/queue"
	for query, want := range map[string][]string{
		"":                            {"ali@uzak.example.net", "zeynep@ornek.example.net"},
		"?status=failed":              {"ali@uzak.example.net", "zeynep@ornek.example.net"},
		"?status=skipped":             {"zeynep@ornek.example.net"},
		"?q=ZEYNEP":                   {"zeynep@ornek.example.net"},
		"?q=uzak.example":             {"ali@uzak.example.net"},
		"?q=ali&status=skipped":       {},
		"?status=sent":                {},
		"?q=can&status=failed&_end=5": {"ali@uzak.example.net"},
	} {
		status, raw, total := searchGet(t, app, searchReader, base+query)
		if status != 200 {
			t.Fatalf("%s: %d %s", query, status, raw)
		}
		var rows []struct {
			Email string `json:"recipient_email"`
		}
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.Email
		}
		if !sameSet(got, want) || total != itoa(len(want)) {
			t.Errorf("%s: %v (X-Total-Count %s); want %v", query, got, total, want)
		}
	}
	if status, raw, _ := searchGet(t, app, searchReader, base+"?status=bounced"); status != 400 || !strings.Contains(raw, "skipped") {
		t.Errorf("unknown status: %d %s", status, raw)
	}
}

func TestTemplateListAndApprovalSearch(t *testing.T) {
	app, store := searchApp(t)
	ctx := context.Background()

	for query, want := range map[string]int{
		"?q=KEYCLOAK":                    1, // key
		"?q=%C5%9Fenli%C4%9Fi":           1, // subject
		"?q=%C5%9Fablon":                 1, // name
		"?q=yok":                         0,
		"?q=keycloak&lifecycle=all":      1,
		"?q=keycloak&lifecycle=inactive": 0,
	} {
		status, raw, total := searchGet(t, app, searchReader, "/v1/templates"+query)
		if status != 200 || total != itoa(want) {
			t.Errorf("templates %s: %d, X-Total-Count %s; want %d: %s", query, status, total, want, raw)
		}
	}

	search := "üyeler"
	lists, err := store.GetAllMailingLists(ctx, database.GetAllMailingListsParams{Limit: 10, Search: &search})
	if err != nil || len(lists) != 1 {
		t.Errorf("lists by name: %v %v", lists, err)
	}
	search = "bütün"
	if count, err := store.CountMailingLists(ctx, &search); err != nil || count != 1 {
		t.Errorf("lists by description: %d %v", count, err)
	}
	search = "yok"
	if count, err := store.CountAllMailingListsIncludingArchived(ctx, &search); err != nil || count != 0 {
		t.Errorf("no list: %d %v", count, err)
	}

	for text, want := range map[string]int64{"fatma": 1, "panel.duyuru": 1, "bahar": 1, "tüm": 0} {
		text := text
		params := database.ListMailApprovalsParams{Limit: 10, Search: &text}
		rows, err := store.ListMailApprovals(ctx, params)
		if err != nil || int64(len(rows)) != want {
			t.Errorf("approvals %q: %d rows %v; want %d", text, len(rows), err, want)
		}
		count, err := store.CountMailApprovals(ctx, database.CountMailApprovalsParams{Search: &text})
		if err != nil || count != want {
			t.Errorf("approvals %q: count %d %v; want %d", text, count, err, want)
		}
	}
}

func TestDailySentIsSplitByStream(t *testing.T) {
	app, _ := searchApp(t)
	// Yesterday: A's 12 (no purpose, bulk lane: duyuru), B's one (system lane,
	// a panel template: islem), C's login code (system). D and E sent nothing
	// that day.
	status, raw, _ := searchGet(t, app, searchReader, "/v1/mail_tasks/summary?days=3")
	if status != 200 {
		t.Fatalf("%d %s", status, raw)
	}
	var summary struct {
		DailySent []struct {
			Sent     int64 `json:"sent"`
			ByStream struct {
				Duyuru int64 `json:"duyuru"`
				Islem  int64 `json:"islem"`
				System int64 `json:"system"`
			} `json:"by_stream"`
		} `json:"daily_sent"`
	}
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatal(err)
	}
	var sent, duyuru, islem, system int64
	for _, day := range summary.DailySent {
		if day.ByStream.Duyuru+day.ByStream.Islem+day.ByStream.System != day.Sent {
			t.Errorf("a day's streams do not add up: %+v", day)
		}
		sent += day.Sent
		duyuru += day.ByStream.Duyuru
		islem += day.ByStream.Islem
		system += day.ByStream.System
	}
	// Queue rows count by created_at (queued now), not sent_at; E's row too.
	if sent != 15 || duyuru != 13 || islem != 1 || system != 1 {
		t.Errorf("sent %d: duyuru %d, islem %d, system %d; want 15: 13, 1, 1", sent, duyuru, islem, system)
	}
}

func sep(query string) string {
	if query == "" {
		return "?"
	}
	return "&"
}

func itoa(n int) string { return strconv.Itoa(n) }

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}
