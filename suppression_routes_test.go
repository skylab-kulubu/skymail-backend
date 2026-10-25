package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	suppressionOperator = "31ef736f-72da-4a40-8791-d523199cf9f0"
	suppressionTask     = "40000000-0000-4000-8000-000000000001"
	refusedRow          = "50000000-0000-4000-8000-000000000001"
	bouncedRow          = "50000000-0000-4000-8000-000000000002"
)

// suppressionApp serves the suppression list and the send pages to a caller
// with roles, over a database with one send: Ayşe sent, then reported as
// not existing (undelivered); Mehmet suppressed when queued; Zeynep's account
// mail sent anyway, flagged.
func suppressionApp(t *testing.T, roles ...string) (*fiber.App, *database.Store) {
	t.Helper()
	postgres := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES ('` + suppressionTask + `', '` + suppressionOperator + `', '{}')`,
		`INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, error, suppressed_but_sent) VALUES
		    ('` + bouncedRow + `', '` + suppressionTask + `', 'Ayşe', 'ayse@uzak.example.net', 'K', 'G', 'sent', NULL, false),
		    ('` + refusedRow + `', '` + suppressionTask + `', 'Mehmet', 'mehmet@uzak.example.net', '', '', 'failed', 'suppressed: smtp RCPT TO 550 5.1.1', false),
		    ('50000000-0000-4000-8000-000000000003', '` + suppressionTask + `', 'Zeynep', 'zeynep@uzak.example.net', 'K', 'G', 'sent', NULL, true)`,
		`INSERT INTO mail_bounces (queue_id, source, kind, action, status, smtp_code, ndr_id_sha256) VALUES
		    ('` + bouncedRow + `', 'dsn', 'hard', 'failed', '5.1.10', 550, repeat('a', 64))`,
		`INSERT INTO mail_suppressions (address, source, reason_code, count, last_queue_id) VALUES
		    ('ayse@uzak.example.net', 'dsn', '5.1.10', 1, '` + bouncedRow + `'),
		    ('mehmet@uzak.example.net', 'smtp', 'RCPT TO 550 5.1.1', 2, NULL),
		    ('zeynep@uzak.example.net', 'dsn', '5.4.1 AS(201806281)', 1, NULL)`,
	} {
		if _, err := postgres.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	store := database.NewStore(postgres.Pool)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", suppressionOperator)
		c.Locals("roles", roles)
		return c.Next()
	})
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid/realms/skylab", middlewares.AudienceOff)
	registerSuppressionRoutes(api, auth, handlers.NewSuppressionHandler(store))
	registerMailTaskRoutes(api, auth, handlers.NewMailHandler(store, idleMailer{}, noGroups{}))
	return app, store
}

func call(t *testing.T, app *fiber.App, method, target string) (*http.Response, string) {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(method, target, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

// Only skymail:suppressions:manage reads or changes the list.
func TestTheSuppressionListNeedsItsRole(t *testing.T) {
	app, _ := suppressionApp(t, "skymail:access", "skymail:mails:read", "skymail:mails:write")
	for _, request := range [][2]string{
		{fiber.MethodGet, "/v1/suppressions"},
		{fiber.MethodDelete, "/v1/suppressions/60000000-0000-4000-8000-000000000001"},
	} {
		if response, _ := call(t, app, request[0], request[1]); response.StatusCode != fiber.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", request[0], request[1], response.StatusCode)
		}
	}
}

func TestTheSuppressionListIsListedAndSearched(t *testing.T) {
	app, _ := suppressionApp(t, "skymail:access", handlers.SuppressionsManageRole)
	response, body := call(t, app, fiber.MethodGet, "/v1/suppressions?_start=0&_end=2")
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "3" {
		t.Fatalf("GET = %d, total %q: %s", response.StatusCode, response.Header.Get("X-Total-Count"), body)
	}
	var page []handlers.Suppression
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Errorf("page = %d rows, want 2", len(page))
	}

	response, body = call(t, app, fiber.MethodGet, "/v1/suppressions?q=MEHMET")
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Total-Count") != "1" || len(page) != 1 || page[0].Address != "mehmet@uzak.example.net" ||
		page[0].Source != "smtp" || page[0].ReasonCode != "RCPT TO 550 5.1.1" || page[0].Count != 2 {
		t.Errorf("search = %s (total %s)", body, response.Header.Get("X-Total-Count"))
	}

	if response, _ := call(t, app, fiber.MethodGet, "/v1/suppressions?q="+strings.Repeat("a", 255)); response.StatusCode != fiber.StatusBadRequest {
		t.Errorf("a search past 254 characters = %d, want 400", response.StatusCode)
	}
}

// Removing names the row by its id, never the address; the removal is
// recorded without the address. A second removal is 404, a malformed id 400.
func TestRemovingASuppressionRecordsWhoWithoutTheAddress(t *testing.T) {
	app, store := suppressionApp(t, "skymail:access", handlers.SuppressionsManageRole)
	ctx := context.Background()
	var id string
	if err := store.Conn.QueryRow(ctx, `SELECT id::text FROM mail_suppressions WHERE address = 'mehmet@uzak.example.net'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if response, body := call(t, app, fiber.MethodDelete, "/v1/suppressions/"+id); response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", response.StatusCode, body)
	}
	var left int
	if err := store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_suppressions WHERE address = 'mehmet@uzak.example.net'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("left %d (%v), want the address removed", left, err)
	}
	var record string
	if err := store.Conn.QueryRow(ctx, `SELECT row_to_json(r)::text FROM mail_suppression_removals r`).Scan(&record); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record, suppressionOperator) || !strings.Contains(record, `"reason_code":"RCPT TO 550 5.1.1"`) || !strings.Contains(record, `"count":2`) {
		t.Errorf("removal record = %s, want who, the code and the count", record)
	}
	if strings.Contains(strings.ToLower(record), "mehmet") {
		t.Errorf("removal record %s names the address", record)
	}

	if response, _ := call(t, app, fiber.MethodDelete, "/v1/suppressions/"+id); response.StatusCode != fiber.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", response.StatusCode)
	}
	if response, _ := call(t, app, fiber.MethodDelete, "/v1/suppressions/mehmet@uzak.example.net"); response.StatusCode != fiber.StatusBadRequest {
		t.Errorf("DELETE by address = %d, want 400", response.StatusCode)
	}
}

// A send's page counts what the suppression list did, and each recipient's
// row says it: suppressed (failed when queued), suppressed_but_sent (account
// mail sent anyway) and undelivered (sent, then reported not to exist). The
// suppressed recipient is skipped in the send's status.
func TestASendCountsItsSuppressedAndUndeliveredRecipients(t *testing.T) {
	app, _ := suppressionApp(t, "skymail:access", "skymail:mails:read")
	response, body := call(t, app, fiber.MethodGet, "/v1/mail_tasks/"+suppressionTask)
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET send = %d: %s", response.StatusCode, body)
	}
	var send struct {
		Status          string `json:"status"`
		RecipientCounts struct {
			Sent              int64 `json:"sent"`
			Failed            int64 `json:"failed"`
			Suppressed        int64 `json:"suppressed"`
			SuppressedButSent int64 `json:"suppressed_but_sent"`
			Undelivered       int64 `json:"undelivered"`
		} `json:"recipient_counts"`
	}
	if err := json.Unmarshal([]byte(body), &send); err != nil {
		t.Fatal(err)
	}
	counts := send.RecipientCounts
	if send.Status != "sent" || counts.Sent != 2 || counts.Failed != 1 || counts.Suppressed != 1 ||
		counts.SuppressedButSent != 1 || counts.Undelivered != 1 {
		t.Errorf("send = %s", body)
	}

	_, body = call(t, app, fiber.MethodGet, "/v1/mail_tasks/"+suppressionTask+"/queue")
	var rows []struct {
		RecipientEmail    string `json:"recipient_email"`
		SuppressedButSent bool   `json:"suppressed_but_sent"`
		Undelivered       bool   `json:"undelivered"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]bool{}
	for _, row := range rows {
		got[row.RecipientEmail] = [2]bool{row.SuppressedButSent, row.Undelivered}
	}
	want := map[string][2]bool{
		"ayse@uzak.example.net":   {false, true},
		"mehmet@uzak.example.net": {false, false},
		"zeynep@uzak.example.net": {true, false},
	}
	for email, flags := range want {
		if got[email] != flags {
			t.Errorf("%s: suppressed_but_sent, undelivered = %v, want %v", email, got[email], flags)
		}
	}
}

// The bounce rate (login-ux spec §8's "bounce > 5%" threshold) and the NDR
// reader's state are read with skymail:suppressions:manage: of the e-mails
// sent in the last days, how many a report said did not reach their address,
// and each mailbox's last poll and counts, without its delta link.
func TestTheBounceRateAndTheReadersStateNeedTheRole(t *testing.T) {
	app, store := suppressionApp(t, handlers.SuppressionsManageRole)
	ctx := context.Background()
	if _, err := store.Conn.Exec(ctx, `
		UPDATE mail_queue SET sent_at = now() - interval '1 day' WHERE status = 'sent';
		INSERT INTO mail_bounce_reader_state (mailbox, mode, delta_link, last_polled_at, reports, hard)
		VALUES ('hesap@bildirim.example.org', 'on', 'https://graph.microsoft.com/v1.0/users/x/mailFolders/inbox/messages/delta?$deltatoken=secret-place', now(), 3, 1)`); err != nil {
		t.Fatal(err)
	}
	response, body := call(t, app, http.MethodGet, "/v1/suppressions/bounces?days=7")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	var got struct {
		Days     int     `json:"days"`
		Sent     int64   `json:"sent"`
		Hard     int64   `json:"hard"`
		Soft     int64   `json:"soft"`
		Delay    int64   `json:"delay"`
		HardRate float64 `json:"hard_rate"`
		Reader   []struct {
			Mailbox string `json:"mailbox"`
			Mode    string `json:"mode"`
			Reports int64  `json:"reports"`
			Hard    int64  `json:"hard"`
		} `json:"reader"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Days != 7 || got.Sent != 2 || got.Hard != 1 || got.HardRate != 0.5 || len(got.Reader) != 1 ||
		got.Reader[0].Mailbox != "hesap@bildirim.example.org" || got.Reader[0].Reports != 3 || got.Reader[0].Hard != 1 {
		t.Errorf("bounces = %+v", got)
	}
	for _, secret := range []string{"deltatoken", "secret-place", "ayse", "@uzak"} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer holds %q: %s", secret, body)
		}
	}
	for _, days := range []string{"0", "91", "x"} {
		if response, _ := call(t, app, http.MethodGet, "/v1/suppressions/bounces?days="+days); response.StatusCode != http.StatusBadRequest {
			t.Errorf("days=%s: status = %d, want 400", days, response.StatusCode)
		}
	}

	other, _ := suppressionApp(t, handlers.UnsubscribesReactivateRole)
	if response, _ := call(t, other, http.MethodGet, "/v1/suppressions/bounces"); response.StatusCode != http.StatusForbidden {
		t.Errorf("without skymail:suppressions:manage: status = %d, want 403", response.StatusCode)
	}
}
