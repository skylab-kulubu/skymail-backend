package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// A person's mail history (skymail-yeni-tasarim B2): GET
// /v1/recipients/history, as production registers it.

const historyReader = "8a3b2c1d-0000-4000-8000-0000000000dd"

// historyApp serves the routes to a reader with roles, on a database holding
// Ayşe's mail: a list send she opened and clicked, a login code (system
// mail) whose content the sweep emptied, a send that skipped her suppressed
// address, a tracked one she has not opened; and Mehmet's mail.
func historyApp(t *testing.T, roles ...string) (*fiber.App, *database.Store) {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content) VALUES
		    ('10000000-0000-4000-8000-000000000001', 'panel.duyuru', 'Duyuru', 'Duyuru', '<p>x</p>', 'x', ''),
		    ('10000000-0000-4000-8000-000000000002', 'keycloak.login-code', 'Giriş kodu', 'Kodunuz', '<p>x</p>', 'x', '');
		INSERT INTO mailing_lists (id, name) VALUES ('20000000-0000-4000-8000-000000000001', 'Tüm üyeler');
		INSERT INTO mail_tasks (id, sent_by, template_id, mail_list_id, body_variables, created_at, content_cleared_at, sent_by_service) VALUES
		    ('40000000-0000-4000-8000-0000000000c1', 'yk-sub', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}', now() - interval '4 days', NULL, NULL),
		    ('40000000-0000-4000-8000-0000000000c2', 'svc-keycloak', '10000000-0000-4000-8000-000000000002', NULL, '{}', now() - interval '3 days', now(), true),
		    ('40000000-0000-4000-8000-0000000000c3', 'yk-sub', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}', now() - interval '2 days', NULL, NULL),
		    ('40000000-0000-4000-8000-0000000000c4', 'yk-sub', '10000000-0000-4000-8000-000000000001', '20000000-0000-4000-8000-000000000001', '{}', now() - interval '1 day', NULL, NULL);
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status, priority, error, created_at, sent_at, tracking_token) VALUES
		    ('50000000-0000-4000-8000-0000000000c1', '40000000-0000-4000-8000-0000000000c1', 'Ayşe Kaya', 'ayse@example.com', 'Bahar şenliği', 'x', '<p>x</p>', 'sent', 0, NULL, now() - interval '4 days', now() - interval '4 days', decode(md5('c1'), 'hex')),
		    ('50000000-0000-4000-8000-0000000000c2', '40000000-0000-4000-8000-0000000000c2', 'Ayşe Kaya', 'Ayse@Example.com', 'Kodunuz', '', NULL, 'sent', 1, NULL, now() - interval '3 days', now() - interval '3 days', NULL),
		    ('50000000-0000-4000-8000-0000000000c3', '40000000-0000-4000-8000-0000000000c3', 'Ayşe Kaya', 'ayse@example.com', 'Kulüp gezisi', 'x', '<p>x</p>', 'failed', 0, 'suppressed: hard_bounce', now() - interval '2 days', NULL, NULL),
		    ('50000000-0000-4000-8000-0000000000c4', '40000000-0000-4000-8000-0000000000c4', 'Ayşe Kaya', 'ayse@example.com', 'Seçim duyurusu', 'x', '<p>x</p>', 'sent', 0, NULL, now() - interval '1 day', now() - interval '1 day', decode(md5('c4'), 'hex')),
		    ('50000000-0000-4000-8000-0000000000c5', '40000000-0000-4000-8000-0000000000c4', 'Mehmet Demir', 'mehmet@example.com', 'Seçim duyurusu', 'x', '<p>x</p>', 'sent', 0, NULL, now() - interval '1 day', now() - interval '1 day', decode(md5('c5'), 'hex'));
		INSERT INTO mail_task_links (id, task_id, link_token, position, url) OVERRIDING SYSTEM VALUE VALUES
		    (1, '40000000-0000-4000-8000-0000000000c1', decode('0102030405060708', 'hex'), 1, 'https://yildizskylab.com/bahar');
		INSERT INTO mail_tracking_events (queue_id, task_id, kind, link_id, source_class, automated_likely, classifier_version, occurred_at) VALUES
		    ('50000000-0000-4000-8000-0000000000c1', '40000000-0000-4000-8000-0000000000c1', 'open', NULL, 'mail_client', true, 1, now() - interval '4 days' + interval '1 minute'),
		    ('50000000-0000-4000-8000-0000000000c1', '40000000-0000-4000-8000-0000000000c1', 'open', NULL, 'mail_client', false, 1, now() - interval '4 days' + interval '1 hour'),
		    ('50000000-0000-4000-8000-0000000000c1', '40000000-0000-4000-8000-0000000000c1', 'click', 1, 'mail_client', false, 1, now() - interval '4 days' + interval '2 hours'),
		    ('50000000-0000-4000-8000-0000000000c5', '40000000-0000-4000-8000-0000000000c4', 'open', NULL, 'mail_client', false, 1, now() - interval '20 hours');`); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", historyReader)
		c.Locals("user_name", "Deniz Okur")
		c.Locals("roles", roles)
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	return app, store
}

func historyCall(t *testing.T, app *fiber.App, query string, out any) (int, string, string) {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/recipients/history?"+query, nil),
		fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if out != nil && response.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	return response.StatusCode, string(raw), response.Header.Get("X-Total-Count")
}

// Every mail queued to Ayşe, whatever case her address was written in (a +
// sent unencoded, read as a blank, is a + again), newest first, with its send, its delivery and her openings and clicks;
// Mehmet's mail is not in it. The read is recorded, by whom and whose (the
// address's SHA-256), before it is answered.
func TestARecipientsHistoryListsHerMail(t *testing.T) {
	app, store := historyApp(t, "skymail:access", "skymail:mails:read")

	var items []handlers.RecipientHistoryItem
	status, raw, total := historyCall(t, app, "email="+url.QueryEscape(" AYSE@example.com "), &items)
	if status != 200 {
		t.Fatalf("history: %d %s", status, raw)
	}
	if total != "4" || len(items) != 4 {
		t.Fatalf("history = %d items, X-Total-Count %q; want Ayşe's 4", len(items), total)
	}
	want := []string{"Seçim duyurusu", "Kulüp gezisi", "Kodunuz", "Bahar şenliği"}
	for i, item := range items {
		if item.Subject != want[i] {
			t.Fatalf("history order = %s, want %s", raw, strings.Join(want, ", "))
		}
	}

	opened := items[3]
	if opened.Status != "sent" || opened.Content != "available" || opened.Send.Kind != "bulk" ||
		opened.Send.MailListName == nil || *opened.Send.MailListName != "Tüm üyeler" || opened.SentAt == nil {
		t.Errorf("the list send she opened = %+v", opened)
	}
	tr := opened.Tracking
	if !tr.Tracked || tr.Opens != 2 || tr.Clicks != 1 || tr.FirstOpenAt == nil || tr.FirstLikelyHumanOpenAt == nil ||
		!tr.FirstLikelyHumanOpenAt.After(*tr.FirstOpenAt) || tr.LastClickedAt == nil {
		t.Errorf("her openings and clicks = %+v", tr)
	}

	if code := items[2]; code.Content != "deleted" || code.Send.Kind != "system" || code.Send.TemplateKey == nil ||
		*code.Send.TemplateKey != "keycloak.login-code" || code.Tracking.Tracked {
		t.Errorf("the login code whose content is gone = %+v", code)
	}
	if skipped := items[1]; skipped.Status != "failed" || !skipped.Skipped || skipped.Error == nil || skipped.SentAt != nil {
		t.Errorf("the send that skipped her = %+v", skipped)
	}
	if unopened := items[0]; !unopened.Tracking.Tracked || unopened.Tracking.Opens != 0 || unopened.Tracking.FirstOpenAt != nil {
		t.Errorf("the send she did not open = %+v (Mehmet's opening must not count)", unopened.Tracking)
	}

	var page []handlers.RecipientHistoryItem
	if status, raw, total := historyCall(t, app, "email=ayse@example.com&_start=1&_end=3", &page); status != 200 || total != "4" ||
		len(page) != 2 || page[0].Subject != "Kulüp gezisi" || page[1].Subject != "Kodunuz" {
		t.Errorf("page 1..3: %d %s total %s", status, raw, total)
	}

	var reads []struct {
		Sub, Name string
		Results   int
	}
	rows, err := store.Conn.Query(context.Background(), `
		SELECT actor_sub, actor_name, results FROM recipient_history_views
		WHERE address_sha256 = encode(sha256('ayse@example.com'::bytea), 'hex') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r struct {
			Sub, Name string
			Results   int
		}
		if err := rows.Scan(&r.Sub, &r.Name, &r.Results); err != nil {
			t.Fatal(err)
		}
		reads = append(reads, r)
	}
	rows.Close()
	if len(reads) != 2 || reads[0].Sub != historyReader || reads[0].Name != "Deniz Okur" || reads[0].Results != 4 {
		t.Errorf("recorded reads = %+v, want both reads by Deniz Okur, 4 results each", reads)
	}
	if n := countOf(t, store, `SELECT count(*) FROM recipient_history_views WHERE address_sha256 !~ '^[0-9a-f]{64}$'`); n != 0 {
		t.Errorf("a read was recorded with something other than a digest")
	}
}

// An address no mail went to: an empty page, counted 0, and recorded.
func TestAnUnknownAddressHasNoHistory(t *testing.T) {
	app, store := historyApp(t, "skymail:access", "skymail:mails:read")
	var items []handlers.RecipientHistoryItem
	if status, raw, total := historyCall(t, app, "email=kimse@example.com", &items); status != 200 || total != "0" || len(items) != 0 || raw != "[]" {
		t.Fatalf("unknown address: %d %s total %s", status, raw, total)
	}
	if n := countOf(t, store, `SELECT count(*) FROM recipient_history_views WHERE results = 0`); n != 1 {
		t.Errorf("the read of an unknown address recorded %d times, want once", n)
	}
}

// skymail:mails:read reads any address's history; nothing less does, and
// nothing is recorded then. An email that is not an address is a validation
// problem whose status is the number.
func TestARecipientsHistoryTakesMailsReadAndAnAddress(t *testing.T) {
	app, store := historyApp(t, "skymail:access", "skymail:mails:write")
	if status, raw, _ := historyCall(t, app, "email=ayse@example.com", nil); status != 403 {
		t.Errorf("without mails:read: %d %s", status, raw)
	}
	if n := countOf(t, store, `SELECT count(*) FROM recipient_history_views`); n != 0 {
		t.Errorf("a refused read was recorded")
	}

	app, _ = historyApp(t, "skymail:access", "skymail:mails:read")
	for _, query := range []string{"", "email=", "email=ayse", "email=%40example.com", "email=a%40b%40c", "email=a%09b%40example.com",
		"email=" + strings.Repeat("a", 320) + "%40example.com"} {
		status, raw, _ := historyCall(t, app, query, nil)
		var problem struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}
		if err := json.Unmarshal([]byte(raw), &problem); err != nil {
			t.Fatal(err)
		}
		if status != 400 || problem.Status != 400 || problem.Code != "validation.error" {
			t.Errorf("%q: %d %s, want 400 validation.error", query, status, raw)
		}
	}
}

func countOf(t *testing.T, store *database.Store, sql string) int {
	t.Helper()
	var n int
	if err := store.Conn.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}
