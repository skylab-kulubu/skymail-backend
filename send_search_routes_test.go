package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
	"github.com/skylab-kulubu/skynotify-backend/internal/handlers"
	"github.com/skylab-kulubu/skynotify-backend/internal/middlewares"
	"github.com/skylab-kulubu/skynotify-backend/internal/migrations"
	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

const (
	searchOperator = "31ef736f-72da-4a40-8791-d523199cf9f0"
	searchOther    = "8a3c1d52-0b7e-4c41-9f0e-2d6a8e5b7c90"

	searchPanelTemplate  = "61000000-0000-4000-8000-000000000001"
	searchSystemTemplate = "61000000-0000-4000-8000-000000000002"
	searchNewsTemplate   = "61000000-0000-4000-8000-000000000003"
	searchList           = "62000000-0000-4000-8000-000000000001"
	searchOtherList      = "62000000-0000-4000-8000-000000000002"

	// A: Tüm üyeler, 12 tracked, 3 likely opened (0.25). B: a person's
	// mail to Ayşe alone, in the system lane, tracked and opened (1.0). C: a
	// login code from before sent_by_service (system by its lane). D: two
	// people, one cancelled (skipped) and one refused. E: an approved send,
	// its tracking rolled up (5 of 10, 0.5). F: core's ops.send-failed
	// notification, in the bulk lane. G: a service's single mail
	// (sent_by_service), in the bulk lane. N: a News send (club.news, a
	// System template; purpose duyuru).
	searchSendA = "63000000-0000-4000-8000-00000000000a"
	searchSendB = "63000000-0000-4000-8000-00000000000b"
	searchSendC = "63000000-0000-4000-8000-00000000000c"
	searchSendD = "63000000-0000-4000-8000-00000000000d"
	searchSendE = "63000000-0000-4000-8000-00000000000e"
	searchSendF = "63000000-0000-4000-8000-00000000000f"
	searchSendG = "63000000-0000-4000-8000-000000000010"
	searchSendN = "63000000-0000-4000-8000-000000000011"
)

// sendLetter names the fixture's sends by letter.
var sendLetter = map[string]string{
	searchSendA: "A", searchSendB: "B", searchSendC: "C", searchSendD: "D",
	searchSendE: "E", searchSendF: "F", searchSendG: "G", searchSendN: "N",
}

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
		    ('` + searchSystemTemplate + `', 'keycloak.login-code', 'Giriş kodu', 'Kodunuz', '<p>x</p>', 'x', '', true),
		    ('` + searchNewsTemplate + `', 'club.news', 'Kulüp haberi', 'Haber', '<p>x</p>', 'x', '', true)`,
		`INSERT INTO mailing_lists (id, name, description) VALUES
		    ('` + searchList + `', 'Tüm üyeler', 'Kulübün bütün üyeleri'),
		    ('` + searchOtherList + `', 'Kayıt_2026', '%100 katılım')`,
		`INSERT INTO notification_types (key, lane) VALUES ('ops.send-failed', 'bulk')`,
		`INSERT INTO notifications (id, type, sent_by, idempotency_key, request_sha256) VALUES
		    ('64000000-0000-4000-8000-000000000001', 'ops.send-failed', 'core-service', 'k1', repeat('a', 64))`,
		`INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, created_at, tracking_requested, sent_by_service, notification_id) VALUES
		    ('` + searchSendA + `', '` + searchOperator + `', '` + searchPanelTemplate + `', '` + searchList + `', '{}', '2026-11-01 10:00+03', true, NULL, NULL),
		    ('` + searchSendB + `', '` + searchOther + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-10 10:00+03', true, false, NULL),
		    ('` + searchSendC + `', 'keycloak-service', '` + searchSystemTemplate + `', NULL, '{}', '2026-11-20 10:00+03', false, NULL, NULL),
		    ('` + searchSendD + `', '` + searchOperator + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-15 23:30+03', false, NULL, NULL),
		    ('` + searchSendE + `', '` + searchOperator + `', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-05 10:00+03', true, NULL, NULL),
		    ('` + searchSendF + `', 'core-service', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-18 10:00+03', false, NULL, '64000000-0000-4000-8000-000000000001'),
		    ('` + searchSendG + `', 'core-service', '` + searchPanelTemplate + `', NULL, '{}', '2026-11-03 10:00+03', false, true, NULL)`,
		`INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, created_at, purpose, category,
		                        source_kind, source_ref, source_version, source_snapshot, source_audience) VALUES
		    ('` + searchSendN + `', '` + searchOperator + `', '` + searchNewsTemplate + `', '{}', '2026-11-12 10:00+03', 'duyuru', 'club:haber',
		     'news', 'news:bahar', 1, '{}', 'all')`,
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
		    ('` + searchSendE + `', 'Ece Su', 'ece@ornek.example.net', 'Bahar şenliği', 'G', 'sent', NULL, 0),
		    ('` + searchSendF + `', 'Operasyon', 'ops@ornek.example.net', '', '', 'pending', NULL, 0),
		    ('` + searchSendG + `', 'Bilet Sahibi', 'bilet@ornek.example.net', '', '', 'pending', NULL, 0),
		    ('` + searchSendN + `', 'Haber Okuru', 'haber@ornek.example.net', 'Haber', 'G', 'sent', NULL, 0)`,
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
	auth := middlewares.NewAuthMiddleware("skynotify", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	registerTemplateRoutes(api, auth, handlers.NewTemplateHandler(store))
	// The list and request lists, as main.go routes them, without their
	// role checks: the search is what is tested here.
	api.Get("/mailing_lists", handlers.NewListHandler(store, noGroups{}).GetLists)
	api.Get("/mail_approvals", handlers.NewMailApprovalHandler(store, nil, noGroups{}, handlers.MailApprovalOptions{}).List)
	return app, store
}

const (
	searchReader  = "skynotify:mails:read,skynotify:templates:read"
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
		letter, ok := sendLetter[s.ID]
		if !ok {
			t.Fatalf("unknown send %s", s.ID)
		}
		out.WriteString(letter)
	}
	return out.String()
}

func TestSendListSearchFiltersAndSorts(t *testing.T) {
	app, _ := searchApp(t)

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"", "CFDNBEGA"},
		{"?q=%C3%BCyeler", "A"},          // the list's name
		{"?q=T%C3%9CM+%C3%9CYELER", "A"}, // Turkish capitals
		{"?q=PANEL.DUYURU", "FDBEGA"},    // the template's key, any case
		{"?q=bahar", "FDBEGA"},           // the template's subject
		{"?q=giri%C5%9F", "C"},           // the System template's name
		{"?q=G%C4%B0R%C4%B0%C5%9E", "C"}, // GİRİŞ
		{"?q=ayse%40ornek", "B"},         // a recipient of a send to people
		{"?q=zeynep", "D"},               // the same, on a send that failed
		{"?q=uye1", ""},                  // a list's recipients are not searched
		{"?q=haber%40ornek", ""},         // nor a News send's
		{"?q=fatma", "E"},                // an approved send's requester
		{"?q=%25", ""},                   // % and _ are text, not patterns
		{"?q=_", ""},
		{"?kind=bulk", "NEA"},   // a News send, an approved send, a list
		{"?kind=system", "CFG"}, // by lane (C), notification (F), service (G)
		{"?kind=single", "DB"},  // B is a person's, though in the system lane
		{"?template_id=" + searchSystemTemplate, "C"},
		{"?list_id=" + searchList, "A"},
		{"?sent_by=" + searchOther, "B"},
		{"?from=2026-11-10&to=2026-11-15", "DNB"}, // to takes the whole day
		{"?from=2026-11-10T10:00:00%2B03:00&to=2026-11-10T10:00:00%2B03:00", "B"},
		{"?from=2026-11-10T10:00:00+03:00&to=2026-11-10T10:00:00+03:00", "B"}, // an unencoded +
		{"?_order=asc", "AGEBNDFC"},
		{"?kind=single&q=bahar&_order=asc", "BD"},
	} {
		status, raw, total := searchGet(t, app, searchReader, "/v1/mail_tasks"+tc.query+sep(tc.query)+"_end=50")
		if status != 200 {
			t.Fatalf("%s: %d %s", tc.query, status, raw)
		}
		if got := letters(t, raw); got != tc.want || total != itoa(len(tc.want)) {
			t.Errorf("%s: sends %q, X-Total-Count %s; want %q, %d", tc.query, got, total, tc.want, len(tc.want))
		}
		// The same filter oldest first goes through the other query
		// (ListMailTaskSendsSorted), whose filters are a copy: the same
		// sends, reversed, the same count.
		if strings.Contains(tc.query, "_order=") {
			continue
		}
		asc := "/v1/mail_tasks" + tc.query + sep(tc.query) + "_order=asc&_end=50"
		status, raw, total = searchGet(t, app, searchReader, asc)
		if status != 200 {
			t.Fatalf("%s: %d %s", asc, status, raw)
		}
		if got, want := letters(t, raw), reversed(tc.want); got != want || total != itoa(len(tc.want)) {
			t.Errorf("%s: sends %q, X-Total-Count %s; want %q, %d", asc, got, total, want, len(tc.want))
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
		kinds[sendLetter[s.ID]] = s.Kind
	}
	if want := map[string]string{"A": "bulk", "B": "single", "C": "system", "D": "single", "E": "bulk",
		"F": "system", "G": "system", "N": "bulk"}; !equalMaps(kinds, want) {
		t.Errorf("kinds %v; want %v", kinds, want)
	}

	// The open rate order tells the counts GET /:id/tracking may withhold:
	// only the tracking role sorts by it. Sends with no rate come last.
	if status, raw, _ := searchGet(t, app, searchReader, "/v1/mail_tasks?_sort=open_rate"); status != 403 || !strings.Contains(raw, "mail_task.sort_forbidden") {
		t.Errorf("open rate without the role: %d %s", status, raw)
	}
	for query, want := range map[string]string{
		"?_sort=open_rate":                      "BEACFDNG", // 1.0, 0.5 (rolled up), 0.25, then newest first
		"?_sort=open_rate&_order=asc":           "AEBGNDFC",
		"?_sort=open_rate&_end=2":               "BE",
		"?_sort=open_rate&_start=2&_end=4":      "AC",
		"?_sort=open_rate&kind=bulk&_order=asc": "AEN",
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
		"?_sort=subject", "?_order=up", "?from=2026-11-20&to=2026-11-10", "?q=" + strings.Repeat("a", 201),
		"?q=a%00b", "?q=%FF%FE", "?sent_by=%00"} {
		if status, raw, _ := searchGet(t, app, searchTracker, "/v1/mail_tasks"+query); status != 400 || !strings.Contains(raw, "validation.error") {
			t.Errorf("%s: %d %s; want 400 validation.error", query, status, raw)
		}
	}
	if status, _, _ := searchGet(t, app, "skynotify:templates:read", "/v1/mail_tasks?q=bahar"); status != 403 {
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
	app, _ := searchApp(t)
	approver := searchReader + ",skynotify:lists:read," + handlers.MailApproverRole

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/v1/templates?q=KEYCLOAK", 1},             // key
		{"/v1/templates?q=%C5%9Fenli%C4%9Fi", 1},    // subject
		{"/v1/templates?q=%C5%9Fablon", 1},          // name
		{"/v1/templates?q=G%C4%B0R%C4%B0%C5%9E", 1}, // GİRİŞ
		{"/v1/templates?q=yok", 0},
		{"/v1/templates?q=keycloak&lifecycle=all", 1},
		{"/v1/templates?q=keycloak&lifecycle=inactive", 0},
		{"/v1/templates?q=%25", 0},
		{"/v1/mailing_lists?q=%C3%BCyeler", 1},     // name
		{"/v1/mailing_lists?q=T%C3%9CM", 1},        // Turkish capitals
		{"/v1/mailing_lists?q=b%C3%BCt%C3%BCn", 1}, // description
		{"/v1/mailing_lists?q=_", 1},               // a literal _, not any one letter
		{"/v1/mailing_lists?q=%25100", 1},          // a literal %
		{"/v1/mailing_lists?q=yok", 0},
		{"/v1/mailing_lists?q=kul%C3%BCb&lifecycle=all", 1},
		{"/v1/mail_approvals?q=fatma", 1}, // requester
		{"/v1/mail_approvals?q=FATMA+DEM%C4%B0R", 1},
		{"/v1/mail_approvals?q=panel.duyuru", 1}, // template key
		{"/v1/mail_approvals?q=bahar", 1},        // template subject
		{"/v1/mail_approvals?q=t%C3%BCm", 0},     // no list on it
		{"/v1/mail_approvals?q=_", 0},
	} {
		status, raw, total := searchGet(t, app, approver, tc.path)
		var rows []json.RawMessage
		if status != 200 || json.Unmarshal([]byte(raw), &rows) != nil || len(rows) != tc.want || total != itoa(tc.want) {
			t.Errorf("%s: %d, %d rows, X-Total-Count %s; want %d: %s", tc.path, status, len(rows), total, tc.want, raw)
		}
	}
	for _, path := range []string{"/v1/templates?q=a%00", "/v1/mailing_lists?q=%FF", "/v1/mail_approvals?q=" + strings.Repeat("%C5%9F", 201)} {
		if status, raw, _ := searchGet(t, app, approver, path); status != 400 || !strings.Contains(raw, "validation.error") {
			t.Errorf("%s: %d %s; want 400", path, status, raw)
		}
	}
}

// The skipped filter and mail_task_status() read the same rows as not sent
// on purpose: a send with one sent row and one failed row is failed exactly
// when its failed row is not in GET /:id/queue?status=skipped.
func TestSkippedRecipientsAreWhatTheSendStatusLeavesOut(t *testing.T) {
	app, store := searchApp(t)
	ctx := context.Background()
	errorsByKind := []string{
		"reserved_domain: not sent", "suppressed: smtp RCPT TO 550", "suppressed: unsubscribed",
		"preference: opted_out", "preference: inactive", "preference: ambiguous", "cancelled",
		"preference: no_account", "smtp: RCPT TO 550 5.1.1", "timeout", "",
	}
	for i, failure := range errorsByKind {
		id := fmt.Sprintf("65000000-0000-4000-8000-%012d", i)
		var errorValue any = failure
		if failure == "" {
			errorValue = nil
		}
		if _, err := store.Conn.Exec(ctx, `INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES ($1, $2, $3, '{}')`,
			id, searchOperator, searchPanelTemplate); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Conn.Exec(ctx, `INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error) VALUES
			($1, 'Gönderilen', 'gonderilen@ornek.example.net', 'K', 'G', 'sent', NULL),
			($1, 'Kalan', 'kalan@ornek.example.net', '', '', 'failed', $2)`, id, errorValue); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := store.Conn.QueryRow(ctx, `SELECT mail_task_status($1)`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		code, raw, total := searchGet(t, app, searchReader, "/v1/mail_tasks/"+id+"/queue?status=skipped")
		if code != 200 {
			t.Fatalf("%q: %d %s", failure, code, raw)
		}
		skipped := total == "1"
		if skipped == (status == "failed") {
			t.Errorf("%q: mail_task_status %s, in skipped %v: the two lists differ", failure, status, skipped)
		}
	}
}

func TestDailySentIsSplitByStream(t *testing.T) {
	app, _ := searchApp(t)
	// A's 12 (no purpose, bulk lane: duyuru), B's one (system lane, a panel
	// template: islem), C's login code (system), E's one (bulk lane: duyuru),
	// N's News mail (purpose duyuru, though its template is a System one). D
	// failed, F and G wait.
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
	// Queue rows count by created_at (queued now), not sent_at.
	if sent != 16 || duyuru != 14 || islem != 1 || system != 1 {
		t.Errorf("sent %d: duyuru %d, islem %d, system %d; want 16: 14, 1, 1", sent, duyuru, islem, system)
	}
}

func sep(query string) string {
	if query == "" {
		return "?"
	}
	return "&"
}

func itoa(n int) string { return strconv.Itoa(n) }

func reversed(letters string) string {
	runes := []rune(letters)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

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

// kindOf is the send list's kind of task (mail_task_kind()).
func (w *approvalWorld) kindOf(task uuid.UUID) string {
	w.t.Helper()
	var kind string
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT mail_task_kind($1)`, task).Scan(&kind); err != nil {
		w.t.Fatal(err)
	}
	return kind
}
